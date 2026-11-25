package server_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestPolicyMachines gives one policy two machines. Each must get its
// own job and its own line of snapshots, with the retention rules
// counted for each, and its own line under Health.
func TestPolicyMachines(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	srv, stop := startServer(t, server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond})
	defer stop()
	base := "https://" + srv.Addr()
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}

	ids := map[string]string{}
	for _, name := range []string{"one", "two", "three"} {
		var tok api.EnrollToken
		e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
		dir := t.TempDir()
		cfg, err := agent.Enroll(dir, base, tok.Token, tok.Fingerprint, name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = cfg.AgentID
		if name == "three" {
			continue // enrolled, in no policy
		}
		ag, err := agent.New(dir, log)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { ag.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
	}

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644)
	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "documents", AgentIDs: []string{ids["one"], ids["two"], ids["one"]},
		Paths: []string{src}, Retention: api.Retention{KeepLast: 1}, Enabled: true}, &pol)
	if len(pol.AgentIDs) != 2 || pol.AgentID != ids["one"] {
		t.Fatalf("machines of the policy: %+v", pol)
	}
	// A policy that names an unknown machine is refused; Microsoft 365
	// and VMware take one machine.
	if err := admin.Do("POST", "/api/v1/policies", api.Policy{Name: "x", AgentIDs: []string{ids["one"], "nobody"}, Paths: []string{src}}, nil); err == nil {
		t.Fatal("an unknown machine was accepted")
	}
	if err := admin.Do("POST", "/api/v1/policies", api.Policy{Name: "x", AgentIDs: []string{ids["one"], ids["two"]}, VMs: []string{"a"}, Hypervisor: "vmware", VMwareServer: "v"}, nil); err == nil {
		t.Fatal("a VMware policy with two machines was accepted")
	}

	run := func() {
		t.Helper()
		var first api.Job
		e.do("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &first)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var jobs []api.Job
			e.do("GET", "/api/v1/jobs?limit=50", nil, &jobs)
			busy := 0
			for _, j := range jobs {
				if j.Status == api.StatusQueued || j.Status == api.StatusRunning {
					busy++
				}
				if j.Status == api.StatusFailed {
					t.Fatalf("job failed: %s", j.Error)
				}
			}
			if busy == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the backups did not finish")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	count := func() map[string]int {
		var snaps []tree.Snapshot
		e.do("GET", "/api/v1/snapshots", nil, &snaps)
		n := map[string]int{}
		for _, s := range snaps {
			n[s.AgentID]++
		}
		return n
	}
	run()
	if n := count(); n[ids["one"]] != 1 || n[ids["two"]] != 1 {
		t.Fatalf("one snapshot for each machine after the first run: %v", n)
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two, longer"), 0o644)
	run()
	if n := count(); n[ids["one"]] != 1 || n[ids["two"]] != 1 {
		t.Fatalf("keep_last 1 keeps one snapshot for each machine: %v", n)
	}

	var h api.Health
	e.do("GET", "/api/v1/health", nil, &h)
	seen := map[string]string{}
	for _, p := range h.Policies {
		seen[p.AgentID] = p.State
		if p.Snapshots != 1 {
			t.Fatalf("health counts each machine's snapshots: %+v", p)
		}
	}
	if len(h.Policies) != 2 || seen[ids["one"]] == "" || seen[ids["two"]] == "" {
		t.Fatalf("a line of health for each machine: %+v", h.Policies)
	}

	// Taking a machine out of the policy stops its backups and keeps
	// its snapshots.
	pol.AgentIDs = []string{ids["two"]}
	e.do("PUT", "/api/v1/policies/"+pol.ID, pol, &pol)
	if pol.AgentID != ids["two"] || len(pol.AgentIDs) != 1 {
		t.Fatalf("after taking a machine out: %+v", pol)
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("three, longer still"), 0o644)
	run()
	if n := count(); n[ids["one"]] != 1 || n[ids["two"]] != 1 {
		t.Fatalf("after taking a machine out: %v", n)
	}
}
