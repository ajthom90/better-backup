package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func rnd(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

type env struct {
	t     *testing.T
	admin *adminclient.Client
}

func (e *env) do(method, path string, in, out any) {
	e.t.Helper()
	if err := e.admin.Do(method, path, in, out); err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
}

// wait blocks until the job leaves the queue and finishes.
func (e *env) wait(id string) *api.Job {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var j api.Job
		e.do("GET", "/api/v1/jobs/"+id, nil, &j)
		if j.Status != api.StatusQueued && j.Status != api.StatusRunning {
			return &j
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("job %s did not finish", id)
	return nil
}

// TestEndToEnd drives a real server and agent over TLS: enroll, back up,
// back up again incrementally, restore in place, apply retention, collect
// garbage, verify, and survive a server restart.
func TestEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	cfg := server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond}
	start := func() (*server.Server, context.CancelFunc, chan error) {
		srv, err := server.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Listen(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { err := srv.Serve(ctx); srv.Close(); done <- err }()
		return srv, cancel, done
	}
	srv, stopServer, serverDone := start()
	cfg.Listen = srv.Addr() // keep the port across the restart below
	base := "https://" + srv.Addr()

	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}

	if _, err := adminclient.New(base, af.Token, "sha256:00"); err == nil {
		t.Fatal("a wrong CA fingerprint was accepted")
	}
	if bad, _ := adminclient.New(base, "wrong-token", af.Fingerprint); bad.Do("GET", "/api/v1/status", nil, nil) == nil {
		t.Fatal("a wrong admin token was accepted")
	}

	// Enroll an agent; the single-use token must not work twice.
	var tok api.EnrollToken
	e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, base, tok.Token, tok.Fingerprint, "test-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Enroll(t.TempDir(), base, tok.Token, tok.Fingerprint, "second"); err == nil {
		t.Fatal("an enrollment token was accepted twice")
	}
	ag, err := agent.New(adir, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { stopAgent(); <-agentDone }()

	var agents []api.Agent
	e.do("GET", "/api/v1/agents", nil, &agents)
	if len(agents) != 1 || agents[0].OS != runtime.GOOS {
		t.Fatalf("agents: %+v", agents)
	}

	src := filepath.Join(t.TempDir(), "docs")
	write(t, filepath.Join(src, "report.txt"), []byte("first version"))
	write(t, filepath.Join(src, "media", "video.bin"), rnd(7, 3<<20))
	write(t, filepath.Join(src, "media", "cache.tmp"), []byte("excluded"))

	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "docs", AgentID: agents[0].ID, Paths: []string{src},
		Excludes: []string{"*.tmp"}, Retention: api.Retention{KeepLast: 2}, Enabled: true}, &pol)

	run := func() *api.Job {
		t.Helper()
		var j api.Job
		e.do("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j)
		done := e.wait(j.ID)
		if done.Status != api.StatusSuccess {
			t.Fatalf("backup job: %s %s %v", done.Status, done.Error, done.Warnings)
		}
		return done
	}
	j1 := run()
	write(t, filepath.Join(src, "report.txt"), []byte("second version, longer"))
	j2 := run()
	var st2 tree.Stats
	if err := jsonUnmarshal(j2.Result, &st2); err != nil || st2.FilesChanged != 1 || st2.BytesUploaded > 1<<20 {
		t.Fatalf("second backup should upload only the changed file: %+v %v", st2, err)
	}

	// Browse the snapshot.
	var ls []api.ListEntry
	e.do("GET", "/api/v1/snapshots/"+j2.SnapshotID+"/ls?path="+url.QueryEscape(filepath.Join(src, "media")), nil, &ls)
	if len(ls) != 1 || ls[0].Name != "video.bin" {
		t.Fatalf("listing: %+v", ls)
	}

	// Restore in place from the first snapshot after losing a file.
	os.Remove(filepath.Join(src, "media", "video.bin"))
	var rj api.Job
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j1.SnapshotID}, &rj)
	if done := e.wait(rj.ID); done.Status != api.StatusSuccess {
		t.Fatalf("restore job: %s %s %v", done.Status, done.Error, done.Warnings)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "media", "video.bin")); !bytes.Equal(b, rnd(7, 3<<20)) {
		t.Fatal("restored file differs from the original")
	}
	if b, _ := os.ReadFile(filepath.Join(src, "report.txt")); string(b) != "first version" {
		t.Fatalf("in-place restore of snapshot 1 left report.txt = %q", b)
	}

	// A third backup pushes the first snapshot out of keep_last=2.
	write(t, filepath.Join(src, "media", "video.bin"), rnd(8, 3<<20))
	run()
	var snaps []tree.Snapshot
	e.do("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 2 {
		t.Fatalf("retention kept %d snapshots, want 2", len(snaps))
	}

	var before, after api.Status
	e.do("GET", "/api/v1/status", nil, &before)
	for _, kind := range []string{"gc", "verify"} {
		var mj api.Job
		e.do("POST", "/api/v1/repo/"+kind, nil, &mj)
		if done := e.wait(mj.ID); done.Status != api.StatusSuccess {
			t.Fatalf("%s: %s %s %s", kind, done.Status, done.Error, done.Result)
		}
	}
	e.do("GET", "/api/v1/status", nil, &after)
	if after.Snapshots != 2 || after.StoredBytes == 0 {
		t.Fatalf("status after gc: %+v (before %+v)", after, before)
	}

	// Restart the server: snapshots persist and the agent reconnects.
	stopServer()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	_, stopServer, serverDone = start()
	defer func() { stopServer(); <-serverDone }()
	out := t.TempDir()
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j2.SnapshotID, Target: out}, &rj)
	if done := e.wait(rj.ID); done.Status != api.StatusSuccess {
		t.Fatalf("restore after restart: %s %s %v", done.Status, done.Error, done.Warnings)
	}
	var found string
	filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Name() == "report.txt" {
			found = p
		}
		return nil
	})
	if b, _ := os.ReadFile(found); string(b) != "second version, longer" {
		t.Fatalf("restore after restart gave %q at %q", b, found)
	}
}
