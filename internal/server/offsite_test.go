package server_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// TestOffsite sets up a copy to a folder through the admin API, runs it
// by hand and on its daily schedule, and checks what the API reports.
func TestOffsite(t *testing.T) {
	data := t.TempDir()
	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Tick: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { err := srv.Serve(ctx); srv.Close(); done <- err }()
	defer func() { cancel(); <-done }()

	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New("https://"+srv.Addr(), af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}

	// Nothing is set up: a run is refused.
	if err := admin.Do("POST", "/api/v1/offsite/run", nil, nil); err == nil {
		t.Fatal("a copy ran without settings")
	}
	// Settings that cannot work are refused.
	for _, bad := range []api.OffsiteSettings{
		{Enabled: true, Kind: "tape"},
		{Enabled: true, Kind: "folder"},
		{Enabled: true, Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "b"},
		{Enabled: true, Kind: "folder", Folder: "/x", DailyAt: "25:99"},
	} {
		if err := admin.Do("PUT", "/api/v1/offsite", bad, nil); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}

	// The secret key is stored but never sent back.
	var st api.OffsiteStatus
	e.do("PUT", "/api/v1/offsite", api.OffsiteSettings{Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "ak", SecretKey: "very-secret"}, &st)
	if st.SecretKey != "" || !st.SecretKeySet {
		t.Fatalf("secret handling: %+v", st.OffsiteSettings)
	}
	e.do("PUT", "/api/v1/offsite", api.OffsiteSettings{Enabled: true, Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "ak", SecretKeySet: true}, &st)
	if !st.SecretKeySet {
		t.Fatal("the stored secret key was dropped on save")
	}

	// A folder that is not there fails the test and the run, with the reason.
	dest := filepath.Join(t.TempDir(), "second-disk")
	e.do("PUT", "/api/v1/offsite", api.OffsiteSettings{Enabled: true, Kind: "folder", Folder: dest}, &st)
	if err := admin.Do("POST", "/api/v1/offsite/test", nil, nil); err == nil {
		t.Fatal("a missing folder passed the test")
	}
	var j api.Job
	e.do("POST", "/api/v1/offsite/run", nil, &j)
	if got := e.wait(j.ID); got.Status != api.StatusFailed || got.Type != api.JobOffsite {
		t.Fatalf("run to a missing folder: %+v", got)
	}

	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	var test api.OffsiteTest
	e.do("POST", "/api/v1/offsite/test", nil, &test)
	if test.Objects != 0 {
		t.Fatalf("test of an empty folder: %+v", test)
	}
	e.do("POST", "/api/v1/offsite/run", nil, &j)
	if got := e.wait(j.ID); got.Status != api.StatusSuccess || !strings.Contains(string(got.Result), `"uploaded":0`) {
		t.Fatalf("run: %+v %s", got, got.Result)
	}
	if _, err := os.Stat(filepath.Join(dest, "config.json")); err != nil {
		t.Fatal(err)
	}
	e.do("GET", "/api/v1/offsite", nil, &st)
	if st.LastSuccess == nil || st.LastJob == nil || st.LastJob.ID != j.ID {
		t.Fatalf("status: %+v", st)
	}

	// Garbage collection and the copy take turns without getting stuck.
	var gc, off api.Job
	e.do("POST", "/api/v1/repo/gc", nil, &gc)
	e.do("POST", "/api/v1/offsite/run", nil, &off)
	if got := e.wait(gc.ID); got.Status != api.StatusSuccess {
		t.Fatalf("gc: %+v", got)
	}
	if got := e.wait(off.ID); got.Status != api.StatusSuccess {
		t.Fatalf("copy after gc: %+v", got)
	}

	// The daily schedule: a slot a minute or two from now does not run
	// yet. The slot is a whole minute, so one minute ahead of 12:00:59
	// would be a second away; two minutes ahead is never under one.
	next := time.Now().Add(2 * time.Minute)
	e.do("PUT", "/api/v1/offsite", api.OffsiteSettings{Enabled: true, Kind: "folder", Folder: dest, DailyAt: next.Format("15:04")}, &st)
	time.Sleep(300 * time.Millisecond)
	e.do("GET", "/api/v1/offsite", nil, &st)
	if st.LastJob.ID != off.ID {
		t.Fatal("the daily copy ran before its time")
	}
}
