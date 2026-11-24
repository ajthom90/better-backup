package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/agentctl"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// TestAgentService runs the agent as it is installed: started before
// the machine is enrolled, and set up over its local control channel.
// It enrolls with a code, reports its state, starts its own backup, is
// unenrolled, and enrolls again from a file an installer left.
func TestAgentService(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
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
	code := func() string {
		var tok api.EnrollToken
		e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
		return api.EncodeEnrollCode(base, tok.Token, tok.Fingerprint)
	}

	// A short path: unix socket names are limited to about 100 bytes.
	adir, err := os.MkdirTemp("", "bba")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(adir)
	svc := agent.NewService(adir, log)
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- svc.Run(actx) }()
	defer func() {
		stopAgent()
		if err := <-agentDone; err != nil {
			t.Error(err)
		}
	}()

	ctx := context.Background()
	ctl := agentctl.Dial(svc.Socket())
	until := func(what string, ok func(*agentctl.Status) bool) *agentctl.Status {
		t.Helper()
		var st *agentctl.Status
		var err error
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if st, err = ctl.Status(ctx); err == nil && ok(st) {
				return st
			}
		}
		t.Fatalf("waiting for %s: status %+v, error %v", what, st, err)
		return nil
	}

	st := until("the control channel", func(*agentctl.Status) bool { return true })
	if st.Enrolled || st.Connected || st.Version == "" {
		t.Fatalf("before enrolling: %+v", st)
	}
	if _, err := ctl.Backup(ctx); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("backup before enrolling: %v", err)
	}
	if _, err := ctl.Enroll(ctx, agentctl.EnrollRequest{Code: "bb1.nonsense"}); err == nil {
		t.Fatal("a nonsense code was accepted")
	}
	if _, err := ctl.Enroll(ctx, agentctl.EnrollRequest{Code: api.EncodeEnrollCode(base, "wrong", af.Fingerprint)}); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("a wrong token: %v", err)
	}

	// Codes are pasted: white space around and inside them is ignored.
	c := code()
	st, err = ctl.Enroll(ctx, agentctl.EnrollRequest{Code: "  " + c[:20] + "\n " + c[20:] + "\n", Name: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enrolled || st.Name != "desk" || st.Server != base {
		t.Fatalf("after enrolling: %+v", st)
	}
	if _, err := ctl.Enroll(ctx, agentctl.EnrollRequest{Code: code()}); err == nil || !strings.Contains(err.Error(), "already enrolled") {
		t.Fatalf("enrolling twice: %v", err)
	}
	st = until("the agent to connect", func(s *agentctl.Status) bool { return s.Connected })
	var agents []api.Agent
	e.do("GET", "/api/v1/agents", nil, &agents)
	if len(agents) != 1 || agents[0].Name != "desk" || agents[0].ID != st.AgentID {
		t.Fatalf("agents: %+v", agents)
	}

	// No policy yet: there is nothing to back up, and it says so.
	if _, err := ctl.Backup(ctx); err == nil || !strings.Contains(err.Error(), "no backup policy") {
		t.Fatalf("backup without a policy: %v", err)
	}
	src := filepath.Join(t.TempDir(), "docs")
	write(t, filepath.Join(src, "a.txt"), rnd(1, 300_000))
	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "docs", AgentID: st.AgentID, Paths: []string{src},
		Schedule: api.Schedule{DailyAt: "02:00"}, Enabled: true}, &pol)
	st = until("the policy", func(s *agentctl.Status) bool { return len(s.Policies) == 1 })
	if p := st.Policies[0]; p.Name != "docs" || p.Schedule != "daily at 02:00" || p.LastFinished != nil || p.Pending {
		t.Fatalf("policy before its first backup: %+v", p)
	}
	res, err := ctl.Backup(ctx)
	if err != nil || res.Started != 1 || res.Policies != 1 {
		t.Fatalf("backup now: %+v, %v", res, err)
	}
	st = until("the backup", func(s *agentctl.Status) bool {
		return len(s.Policies) == 1 && s.Policies[0].LastSuccess != nil && s.Job == nil
	})
	if p := st.Policies[0]; p.LastStatus != api.StatusSuccess || p.Pending || p.LastError != "" {
		t.Fatalf("policy after its backup: %+v", p)
	}
	var snaps []struct{ ID string }
	e.do("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 1 {
		t.Fatalf("snapshots: %+v", snaps)
	}

	// The server turns the machine away: the agent says so.
	e.do("DELETE", "/api/v1/agents/"+st.AgentID, nil, nil)
	until("the revocation to show", func(s *agentctl.Status) bool { return s.Revoked && !s.Connected })

	if err := ctl.Unenroll(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = ctl.Status(ctx)
	if err != nil || st.Enrolled || agent.Enrolled(adir) {
		t.Fatalf("after unenrolling: %+v, %v", st, err)
	}

	// An installer leaves a code that the server refuses: the agent
	// says why and drops the file.
	preseed := filepath.Join(adir, agent.PreseedFile)
	write(t, preseed, []byte("[enroll]\r\ncode="+api.EncodeEnrollCode(base, "stale", af.Fingerprint)+"\r\n"))
	st = until("the refused code", func(s *agentctl.Status) bool { return s.Error != "" })
	if !strings.Contains(st.Error, "refused") || st.Enrolled {
		t.Fatalf("refused preseed: %+v", st)
	}
	if _, err := os.Stat(preseed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused preseed file is still there: %v", err)
	}
	// And one that works, as the Windows installer writes it.
	write(t, preseed, []byte("\ufeff[enroll]\r\ncode = "+code()+"\r\nname = \"front desk\"\r\n"))
	st = until("the preseeded enrollment", func(s *agentctl.Status) bool { return s.Connected })
	if st.Name != "front desk" || st.Error != "" {
		t.Fatalf("preseeded: %+v", st)
	}
	if _, err := os.Stat(preseed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preseed file is still there: %v", err)
	}
}

func TestEnrollCode(t *testing.T) {
	c := api.EncodeEnrollCode("https://backup.example.com:8443/", "tok", "sha256:ab")
	s, tok, fp, err := api.DecodeEnrollCode(" " + c + "\n")
	if err != nil || s != "https://backup.example.com:8443" || tok != "tok" || fp != "sha256:ab" {
		t.Fatalf("%q %q %q %v", s, tok, fp, err)
	}
	for _, bad := range []string{"", "tok", "bb1.", "bb1.!!!", "bb1.e30", "bb2." + c[4:]} {
		if _, _, _, err := api.DecodeEnrollCode(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
