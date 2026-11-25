package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/agentctl"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

func sum(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestAgentUpdate follows an agent through updating itself from an
// installer the server holds: not while the setting is off, then a try
// that fails and is rolled back, with a backup that waits for it, and
// then "Update now", which works.
func TestAgentUpdate(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	kind := map[string]string{"windows": api.InstallMSI, "darwin": api.InstallPkg}[runtime.GOOS]
	arch := runtime.GOARCH
	if kind == "" {
		kind = api.InstallDeb
	}
	if runtime.GOOS == "darwin" {
		arch = "universal"
	}
	// What the server has to hand out: an older and a newer installer for
	// this machine, and one for another kind of machine.
	write(t, filepath.Join(data, "installers", "better-backup-agent_1.5.0_"+runtime.GOOS+"_"+arch+"."+kind), rnd(1, 2000))
	write(t, filepath.Join(data, "installers", "better-backup-agent_2.0.0_"+runtime.GOOS+"_"+arch+"."+kind), rnd(2, 300_000))
	write(t, filepath.Join(data, "installers", "better-backup-agent_3.0.0_linux_s390x.rpm"), rnd(3, 2000))

	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	sctx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan struct{})
	go func() { srv.Serve(sctx); srv.Close(); close(serverDone) }()
	defer func() { stopServer(); <-serverDone }()
	base := "https://" + srv.Addr()
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}

	adir, err := os.MkdirTemp("", "bbu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(adir)

	// The updater is played here: its installer does what the test says.
	var mu sync.Mutex
	starts := 0
	install := func(context.Context, string, string, string) error { return nil }
	updaterDone := make(chan error, 4)
	undo := agent.SetUpdateHooks(agent.UpdateHooks{Install: kind, FirstCheck: 100 * time.Millisecond, Settle: 100 * time.Millisecond, HealthWait: 30 * time.Second,
		Start: func(exe string, args []string) error {
			if _, err := os.Stat(exe); err != nil {
				t.Errorf("no copy of the program was kept for the updater: %v", err)
			}
			mu.Lock()
			starts++
			f := install
			mu.Unlock()
			u := &agent.Updater{Dir: adir, Log: log, Install: f, Stop: func() error { return nil }, Restart: func() error { return nil }}
			go func() { updaterDone <- u.Run(context.Background()) }()
			return nil
		}})
	defer undo()
	oldVersion := agent.Version
	agent.Version = "1.0.0"
	defer func() { agent.Version = oldVersion }()

	var stopAgent context.CancelFunc
	var agentDone chan error
	startAgent := func() {
		svc := agent.NewService(adir, log)
		var actx context.Context
		actx, stopAgent = context.WithCancel(context.Background())
		agentDone = make(chan error, 1)
		go func() { agentDone <- svc.Run(actx) }()
	}
	haltAgent := func() {
		stopAgent()
		if err := <-agentDone; err != nil {
			t.Error(err)
		}
	}
	state := func() *api.Agent {
		var agents []*api.Agent
		e.do("GET", "/api/v1/agents", nil, &agents)
		if len(agents) != 1 || agents[0].Update == nil {
			t.Fatalf("agents: %+v", agents)
		}
		return agents[0]
	}
	until := func(what string, ok func(*api.Agent) bool) *api.Agent {
		t.Helper()
		var a *api.Agent
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if a = state(); ok(a) {
				return a
			}
		}
		t.Fatalf("waiting for %s: %+v, update %+v", what, a, a.Update)
		return nil
	}

	// Updates are off: the agent is told nothing, and the page can say
	// what there would be.
	var set api.AgentUpdateSettings
	e.do("PUT", "/api/v1/agent-updates", api.AgentUpdateSettings{Auto: false}, &set)
	if set.Auto || set.Newest != "3.0.0" {
		t.Fatalf("settings: %+v", set)
	}
	var tok api.EnrollToken
	e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
	if _, err := agent.EnrollWith(adir, agentctl.EnrollRequest{Code: api.EncodeEnrollCode(base, tok.Token, tok.Fingerprint), Name: "desk"}); err != nil {
		t.Fatal(err)
	}
	startAgent()
	a := until("the agent to say how it was installed", func(a *api.Agent) bool { return a.Update.Install == kind })
	if u := a.Update; a.Version != "1.0.0" || u.Available != "2.0.0" || !u.Can || u.Auto || u.State != "" {
		t.Fatalf("with updates off: version %s, %+v", a.Version, u)
	}
	time.Sleep(500 * time.Millisecond)
	if mu.Lock(); starts != 0 {
		t.Fatal("the agent updated although updates are off")
	}
	mu.Unlock()

	// A policy, so that a backup can be asked for while the update runs.
	src := filepath.Join(t.TempDir(), "docs")
	write(t, filepath.Join(src, "a.txt"), rnd(4, 100_000))
	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "docs", AgentID: a.ID, Paths: []string{src}, Enabled: true}, &pol)

	// Turned on: the agent fetches the installer and installs it. This
	// installer fails, after a while.
	installing, letFail := make(chan struct{}), make(chan struct{})
	mu.Lock()
	install = func(_ context.Context, k, installer, _ string) error {
		if k != kind || !strings.HasSuffix(installer, "better-backup-agent_2.0.0_"+runtime.GOOS+"_"+arch+"."+kind) {
			t.Errorf("installing %s %s", k, installer)
		}
		if got, want := sum(t, installer), sum(t, filepath.Join(data, "installers", filepath.Base(installer))); got != want {
			t.Error("the installer the agent fetched is not the server's")
		}
		close(installing)
		<-letFail
		return errors.New("exit status 1: no room on the disk")
	}
	mu.Unlock()
	e.do("PUT", "/api/v1/agent-updates", api.AgentUpdateSettings{Auto: true}, &set)
	select {
	case <-installing:
	case <-time.After(60 * time.Second):
		t.Fatalf("the agent did not start installing: %+v", state().Update)
	}
	a = until("the server to hear of the install", func(a *api.Agent) bool { return a.Update.State == api.UpdateInstalling })
	if a.Update.Target != "2.0.0" || !a.Update.Auto {
		t.Fatalf("installing: %+v", a.Update)
	}
	// A backup asked for now waits: the installer would cut it short.
	var j api.Job
	e.do("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j)
	time.Sleep(time.Second)
	e.do("GET", "/api/v1/jobs/"+j.ID, nil, &j)
	if j.Status != api.StatusQueued {
		t.Fatalf("a backup started in the middle of an update: %s", j.Status)
	}
	close(letFail)
	if err := <-updaterDone; err == nil {
		t.Fatal("the updater says the failed install worked")
	}
	a = until("the failure to show", func(a *api.Agent) bool { return a.Update.State == api.UpdateFailed })
	if u := a.Update; !strings.Contains(u.Error, "no room on the disk") || !strings.Contains(u.Error, "put back") || a.Version != "1.0.0" || u.Available != "2.0.0" {
		t.Fatalf("after the failure: version %s, %+v", a.Version, u)
	}
	// The backup that waited runs now, on the version that was put back.
	if done := e.wait(j.ID); done.Status != api.StatusSuccess {
		t.Fatalf("the backup after the failed update: %s %s", done.Status, done.Error)
	}
	// And the agent does not try the same version again and again.
	e.do("PUT", "/api/v1/agent-updates", api.AgentUpdateSettings{Auto: true}, &set) // has every agent ask again
	time.Sleep(time.Second)
	if mu.Lock(); starts != 1 {
		t.Fatalf("the updater was started %d times", starts)
	}
	mu.Unlock()

	// "Update now" tries at once. This time the installer works: the
	// agent stops and comes back as the new version.
	installed := make(chan struct{})
	mu.Lock()
	install = func(context.Context, string, string, string) error { close(installed); return nil }
	mu.Unlock()
	var st api.AgentUpdateState
	e.do("POST", "/api/v1/agents/"+a.ID+"/update", nil, &st)
	if !st.Asked {
		t.Fatalf("after update now: %+v", st)
	}
	select {
	case <-installed:
	case <-time.After(60 * time.Second):
		t.Fatalf("the agent did not start installing after update now: %+v", state().Update)
	}
	haltAgent()
	time.Sleep(200 * time.Millisecond)
	agent.Version = "2.0.0"
	startAgent()
	defer haltAgent()
	if err := <-updaterDone; err != nil {
		t.Fatalf("the updater: %v", err)
	}
	a = until("the new version", func(a *api.Agent) bool { return a.Update.State == api.UpdateDone })
	if u := a.Update; a.Version != "2.0.0" || u.Available != "" || u.Asked || u.Error != "" || u.Target != "2.0.0" {
		t.Fatalf("after the update: version %s, %+v", a.Version, u)
	}
	// One agent can be kept from updating by itself.
	e.do("PUT", "/api/v1/agents/"+a.ID+"/update-mode", map[string]string{"mode": api.AutoUpdateOff}, &st)
	if st.Auto || st.Mode != api.AutoUpdateOff {
		t.Fatalf("mode off: %+v", st)
	}
	if err := admin.Do("POST", "/api/v1/agents/"+a.ID+"/update", nil, nil); err == nil || !strings.Contains(err.Error(), "newest version") {
		t.Fatalf("update now with nothing newer: %v", err)
	}
	var audit []api.AuditEntry
	e.do("GET", "/api/v1/audit", nil, &audit)
	seen := map[string]bool{}
	for _, entry := range audit {
		seen[entry.Action] = true
	}
	for _, want := range []string{"An agent's update failed", "An agent updated itself", "Told an agent to update now", "Changed whether agents update by themselves"} {
		if !seen[want] {
			t.Errorf("the activity log lacks %q", want)
		}
	}
}
