package server_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// TestBrowse asks a running agent what is on its machine, the way the
// policy dialog does, and checks what an agent that is away answers.
func TestBrowse(t *testing.T) {
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

	enroll := func(name string) string {
		var tok api.EnrollToken
		e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
		dir := t.TempDir()
		if _, err := agent.Enroll(dir, base, tok.Token, tok.Fingerprint, name); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	adir := enroll("here")
	enroll("away")
	ag, err := agent.New(adir, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ag.Run(actx); close(done) }()
	defer func() { stopAgent(); <-done }()

	var agents []api.Agent
	ids := map[string]string{}
	deadline := time.Now().Add(20 * time.Second)
	for {
		e.do("GET", "/api/v1/agents", nil, &agents)
		online := false
		for _, a := range agents {
			ids[a.Name] = a.ID
			online = online || a.Name == "here" && a.Online
		}
		if online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent did not connect")
		}
		time.Sleep(50 * time.Millisecond)
	}

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Shares", "Sales"), 0o755)
	os.WriteFile(filepath.Join(root, "a-file.txt"), []byte("hello"), 0o644)

	var res api.BrowseResult
	e.do("POST", "/api/v1/agents/"+ids["here"]+"/browse", api.BrowseRequest{Kind: api.BrowseFolders, Path: root}, &res)
	if res.Error != "" || len(res.Entries) != 2 {
		t.Fatalf("listing %s: %+v", root, res)
	}
	if f := res.Entries[0]; f.Name != "Shares" || !f.Dir || f.Value != filepath.Join(root, "Shares") {
		t.Fatalf("folders come first, with their full path: %+v", res.Entries)
	}
	if f := res.Entries[1]; f.Name != "a-file.txt" || f.Dir || f.Size != 5 {
		t.Fatalf("the file: %+v", f)
	}
	if res.Up == nil || *res.Up != filepath.Dir(root) {
		t.Fatalf("the folder above: %v", res.Up)
	}

	// Where to start, and a folder that is not there.
	res = api.BrowseResult{}
	e.do("POST", "/api/v1/agents/"+ids["here"]+"/browse", api.BrowseRequest{Kind: api.BrowseFolders}, &res)
	if res.Error != "" || len(res.Entries) == 0 {
		t.Fatalf("the top: %+v", res)
	}
	res = api.BrowseResult{}
	e.do("POST", "/api/v1/agents/"+ids["here"]+"/browse", api.BrowseRequest{Kind: api.BrowseFolders, Path: filepath.Join(root, "missing")}, &res)
	if res.Error == "" {
		t.Fatal("a missing folder was listed")
	}

	// Volumes, disks and virtual machines answer or explain; which,
	// depends on the machine the test runs on.
	for _, kind := range []string{api.BrowseVolumes, api.BrowseDisks, api.BrowseVMs} {
		res = api.BrowseResult{}
		e.do("POST", "/api/v1/agents/"+ids["here"]+"/browse", api.BrowseRequest{Kind: kind}, &res)
		if res.Entries == nil {
			t.Fatalf("%s: no list in %+v", kind, res)
		}
		t.Logf("%s: %d entries, error %q", kind, len(res.Entries), res.Error)
	}

	// An agent that never asks for questions (it is away, or older than
	// this) is not waited for long.
	defer server.SetBrowseWaitUnseen(300 * time.Millisecond)()
	err = admin.Do("POST", "/api/v1/agents/"+ids["away"]+"/browse", api.BrowseRequest{Kind: api.BrowseFolders}, &res)
	if err == nil || !strings.Contains(err.Error(), "has not answered") {
		t.Fatalf("browsing an agent that is away: %v", err)
	}
	if err := admin.Do("POST", "/api/v1/agents/"+ids["here"]+"/browse", api.BrowseRequest{Kind: "secrets"}, &res); err == nil {
		t.Fatal("an unknown kind of list was accepted")
	}
	// Only a Windows domain controller can be restarted into its restore
	// mode; any other machine says so and is left alone.
	if runtime.GOOS != "windows" {
		err = admin.Do("POST", "/api/v1/agents/"+ids["here"]+"/dsrm", map[string]bool{"leave": false}, nil)
		if err == nil || !strings.Contains(err.Error(), "domain controller") {
			t.Fatalf("restore mode on a machine that has none: %v", err)
		}
	}
	if err := admin.Do("POST", "/api/v1/agents/nobody/browse", api.BrowseRequest{Kind: api.BrowseFolders}, &res); err == nil {
		t.Fatal("an unknown agent was browsed")
	}
}
