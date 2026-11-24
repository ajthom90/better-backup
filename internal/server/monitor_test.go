package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// TestMonitoring covers the health report, cancelling jobs, the checks
// the server schedules for itself, the summary and the webhook.
func TestMonitoring(t *testing.T) {
	data := t.TempDir()
	srv, _ := startServer(t, server.Config{DataDir: data, Listen: "127.0.0.1:0", Tick: 50 * time.Millisecond, Log: quiet})
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	w := newWeb(t, srv.Addr()).as(af.Token)

	// A webhook receives what email would.
	var mu sync.Mutex
	var hooks []map[string]string
	hook := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		json.Unmarshal(b, &m)
		mu.Lock()
		hooks = append(hooks, m)
		mu.Unlock()
	}))
	defer hook.Close()
	hooked := func(part string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(hooks, func(m map[string]string) bool {
			return strings.Contains(m["text"], part) && m["content"] != "" && m["subject"] != ""
		})
	}
	w.refused(400, "PUT", "/api/v1/alerts", api.AlertSettings{Webhook: "ftp://example.com/x"})
	w.refused(400, "PUT", "/api/v1/alerts", api.AlertSettings{Summary: "hourly"})
	w.refused(400, "POST", "/api/v1/alerts/test-webhook", nil)
	w.must("PUT", "/api/v1/alerts", api.AlertSettings{Webhook: hook.URL, Summary: "daily", SummaryAt: "07:00"}, nil)
	w.must("POST", "/api/v1/alerts/test-webhook", nil, nil)
	if !hooked("Test message") {
		t.Fatal("the webhook did not get the test message")
	}

	// A new server: nothing failed, but several things deserve a look.
	var h api.Health
	w.must("GET", "/api/v1/health", nil, &h)
	texts := func() string {
		var all []string
		for _, i := range h.Issues {
			all = append(all, i.Level+": "+i.Text)
		}
		return strings.Join(all, "\n")
	}
	if h.State != "warning" || !strings.Contains(texts(), "no offsite copy") || !strings.Contains(texts(), "repository key") || strings.Contains(texts(), "webhook") {
		t.Fatalf("health of a new server: %s\n%s", h.State, texts())
	}
	if !h.Disk.Known || h.Disk.Free <= 0 || h.Disk.Total < h.Disk.Free {
		t.Fatalf("disk: %+v", h.Disk)
	}
	if h.NextVerify == nil || h.NextVerify.Before(time.Now().Add(28*24*time.Hour)) || h.NextVerify.After(time.Now().Add(31*24*time.Hour)) {
		t.Fatalf("the first check of all data should be about 30 days away: %v", h.NextVerify)
	}
	waitFor(t, "today's usage", func() bool { h = api.Health{}; w.must("GET", "/api/v1/health", nil, &h); return len(h.Usage) == 1 })

	// Settings for the server's own checks.
	var m api.MonitorSettings
	w.must("GET", "/api/v1/monitor", nil, &m)
	if m.VerifyEveryDays != 30 || m.DiskWarnPercent != 10 {
		t.Fatalf("defaults: %+v", m)
	}
	w.refused(400, "PUT", "/api/v1/monitor", api.MonitorSettings{VerifyAt: "25:00"})
	w.refused(400, "PUT", "/api/v1/monitor", api.MonitorSettings{DiskWarnPercent: 95})
	w.must("PUT", "/api/v1/monitor", api.MonitorSettings{VerifyEveryDays: 0, VerifyAt: "02:30", DiskWarnPercent: 10}, nil)
	h = api.Health{}
	w.must("GET", "/api/v1/health", nil, &h)
	if h.NextVerify != nil {
		t.Fatal("a check is planned although it is turned off")
	}
	// A disk that counts as nearly full: warn at 90% free cannot be set,
	// so ask for the most allowed and see whether this disk is below it.
	w.must("PUT", "/api/v1/monitor", api.MonitorSettings{VerifyEveryDays: 30, VerifyAt: "02:30", DiskWarnPercent: 90}, nil)
	h = api.Health{}
	w.must("GET", "/api/v1/health", nil, &h)
	if low := h.Disk.Free*100 < h.Disk.Total*90; low != strings.Contains(texts(), "nearly full") {
		t.Fatalf("disk warning: free %d of %d, issues:\n%s", h.Disk.Free, h.Disk.Total, texts())
	}
	w.must("PUT", "/api/v1/monitor", api.MonitorSettings{VerifyEveryDays: 30, VerifyAt: "02:30", DiskWarnPercent: 10}, nil)

	// An agent that is enrolled but never runs: its jobs wait.
	var tok api.EnrollToken
	w.must("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, w.base, tok.Token, tok.Fingerprint, "laptop"); err != nil {
		t.Fatal(err)
	}
	var agents []api.Agent
	w.must("GET", "/api/v1/agents", nil, &agents)
	h = api.Health{}
	w.must("GET", "/api/v1/health", nil, &h)
	if !strings.Contains(texts(), "laptop has an agent but no backup policy") {
		t.Fatalf("an agent without a policy:\n%s", texts())
	}
	src := t.TempDir()
	// Big enough that the backup is still running when it is cancelled.
	write(t, filepath.Join(src, "big.bin"), rnd(7, 48<<20))
	var pol api.Policy
	w.must("POST", "/api/v1/policies", api.Policy{Name: "documents", AgentID: agents[0].ID, Paths: []string{src}, Enabled: true}, &pol)
	var j api.Job
	w.must("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j)
	w.must("POST", "/api/v1/jobs/"+j.ID+"/cancel", nil, &j)
	if j.Status != api.StatusCancelled || !strings.Contains(j.Error, "Cancelled by admin token") {
		t.Fatalf("cancelling a waiting job: %+v", j)
	}
	w.refused(409, "POST", "/api/v1/jobs/"+j.ID+"/cancel", nil)
	w.refused(404, "POST", "/api/v1/jobs/nope/cancel", nil)

	// The agent starts; a running backup is cancelled and the agent
	// carries on with the next job.
	ag, err := agent.New(adir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { stopAgent(); <-agentDone }()
	w.must("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j)
	waitFor(t, "the backup to start", func() bool { w.must("GET", "/api/v1/jobs/"+j.ID, nil, &j); return j.Status != api.StatusQueued })
	if j.Status == api.StatusRunning {
		w.must("POST", "/api/v1/jobs/"+j.ID+"/cancel", nil, &j)
		if j.Status != api.StatusCancelled {
			t.Fatalf("cancelling a running job: %+v", j)
		}
		var snaps []json.RawMessage
		w.must("GET", "/api/v1/snapshots", nil, &snaps)
		if len(snaps) != 0 {
			t.Fatal("a cancelled backup left a snapshot")
		}
	} else {
		t.Log("the backup finished before it could be cancelled")
	}
	write(t, filepath.Join(src, "small.txt"), rnd(8, 1000))
	var j2 api.Job
	w.must("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j2)
	waitFor(t, "the next backup", func() bool {
		w.must("GET", "/api/v1/jobs/"+j2.ID, nil, &j2)
		return j2.Status != api.StatusQueued && j2.Status != api.StatusRunning
	})
	if j2.Status != api.StatusSuccess {
		t.Fatalf("the backup after a cancelled one: %s %s", j2.Status, j2.Error)
	}
	h = api.Health{}
	w.must("GET", "/api/v1/health", nil, &h)
	if len(h.Policies) != 1 || h.Policies[0].State != "paused" || h.Policies[0].LastGood == nil || h.Policies[0].Size < 48<<20 || h.Policies[0].Snapshots != 1 || h.Policies[0].Agent != "laptop" {
		t.Fatalf("policy health: %+v", h.Policies)
	}
	if h.Jobs[api.StatusSuccess] != 1 || h.Jobs[api.StatusCancelled] < 1 {
		t.Fatalf("job counts: %v", h.Jobs)
	}

	// A check that is running cannot be stopped; one that waits can.
	// (Nothing waits here for long, so only the refusal's wording is
	// checked when the check happens to be caught running.)
	var v api.Job
	w.must("POST", "/api/v1/repo/verify", nil, &v)
	if code, msg := w.try("POST", "/api/v1/jobs/"+v.ID+"/cancel", nil, nil); code == 409 && !strings.Contains(msg, "cannot be stopped") && !strings.Contains(msg, "already finished") {
		t.Fatal(msg)
	}
	waitFor(t, "the check", func() bool {
		w.must("GET", "/api/v1/jobs/"+v.ID, nil, &v)
		return v.Status != api.StatusQueued && v.Status != api.StatusRunning
	})
	h = api.Health{}
	w.must("GET", "/api/v1/health", nil, &h)
	if v.Status == api.StatusSuccess && h.LastVerify == nil {
		t.Fatal("the health report does not know about the check")
	}

	// The summary names the policy and what needs attention.
	var sum struct{ Subject, Body string }
	w.must("POST", "/api/v1/alerts/summary", nil, &sum)
	for _, want := range []string{"documents on laptop", "Needs attention", "no offsite copy", "1 succeeded", "Repository:"} {
		if !strings.Contains(sum.Body, want) {
			t.Errorf("the summary lacks %q:\n%s", want, sum.Body)
		}
	}
	if !strings.Contains(sum.Subject, "Daily summary") {
		t.Error(sum.Subject)
	}

	// A failed backup reaches the webhook, and shows in the health report.
	w.must("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "documents", AgentID: agents[0].ID, Paths: []string{filepath.Join(src, "missing")}, Enabled: true}, nil)
	var j3 api.Job
	w.must("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j3)
	waitFor(t, "the failing backup", func() bool {
		w.must("GET", "/api/v1/jobs/"+j3.ID, nil, &j3)
		return j3.Status != api.StatusQueued && j3.Status != api.StatusRunning
	})
	if j3.Status == api.StatusFailed {
		waitFor(t, "the webhook to hear of the failure", func() bool { return hooked("Backup failed: documents on laptop") })
		h = api.Health{}
		w.must("GET", "/api/v1/health", nil, &h)
		if h.State != "failed" || h.Policies[0].State != "failed" {
			t.Fatalf("health after a failed backup: %s %+v", h.State, h.Policies)
		}
	} else {
		t.Logf("a backup of a missing folder ended as %s; failure path not exercised", j3.Status)
	}
}
