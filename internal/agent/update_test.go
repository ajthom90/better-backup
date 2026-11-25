package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// planIn writes the plan of an update under way and returns the "installed
// program" the plan names, which holds the new version.
func planIn(t *testing.T, dir string) string {
	t.Helper()
	exe := filepath.Join(dir, "installed-agent")
	if err := os.MkdirAll(filepath.Join(dir, updateFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("the new version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := savePlan(dir, &updatePlan{From: "1.0.0", To: "2.0.0", Install: api.InstallDeb, Installer: "x.deb", Exe: exe, State: planInstalling, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return exe
}

func sameAsSelf(t *testing.T, path string) bool {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fileSHA256(path) == fileSHA256(self)
}

// TestUpdaterRollsBack: when the installer fails, or the new version
// never reaches the server, the earlier program is put back and started,
// and the plan says what happened.
func TestUpdaterRollsBack(t *testing.T) {
	defer SetUpdateHooks(UpdateHooks{HealthWait: 300 * time.Millisecond})()
	for name, install := range map[string]func(context.Context, string, string, string) error{
		"the installer fails":           func(context.Context, string, string, string) error { return errors.New("exit status 1: disk full") },
		"the new version does not come": func(context.Context, string, string, string) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			exe := planIn(t, dir)
			stopped, restarted := 0, 0
			u := &Updater{Dir: dir, Log: quiet(), Install: install,
				Stop:    func() error { stopped++; return nil },
				Restart: func() error { restarted++; return nil }}
			err := u.Run(context.Background())
			if err == nil {
				t.Fatal("the update is said to have worked")
			}
			if !sameAsSelf(t, exe) {
				t.Error("the earlier program was not put back")
			}
			if restarted != 1 {
				t.Errorf("the agent was started %d times", restarted)
			}
			p := loadPlan(dir)
			if p == nil || p.State != planFailed || !strings.Contains(p.Error, "Version 1.0.0 was put back") || p.Reported {
				t.Fatalf("plan: %+v", p)
			}
			if name == "the installer fails" && !strings.Contains(p.Error, "disk full") {
				t.Errorf("the error does not say why: %s", p.Error)
			}
		})
	}
}

// TestUpdaterSucceeds: once the new version says it reached the server,
// the updater leaves it alone.
func TestUpdaterSucceeds(t *testing.T) {
	defer SetUpdateHooks(UpdateHooks{HealthWait: 20 * time.Second})()
	dir := t.TempDir()
	exe := planIn(t, dir)
	u := &Updater{Dir: dir, Log: quiet(),
		Install: func(context.Context, string, string, string) error {
			go func() {
				time.Sleep(200 * time.Millisecond)
				p := loadPlan(dir)
				p.State = planOK
				savePlan(dir, p)
			}()
			return nil
		},
		Stop:    func() error { t.Error("stopped the agent"); return nil },
		Restart: func() error { t.Error("restarted the agent"); return nil }}
	if err := u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "the new version" {
		t.Error("the new program was replaced")
	}
}

// TestFetchInstallerChecks: an installer that is not byte for byte what
// the server described is thrown away.
func TestFetchInstallerChecks(t *testing.T) {
	good := bytes.Repeat([]byte("installer "), 1000)
	served := good
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(served) }))
	defer ts.Close()
	a := &Agent{c: &client{base: ts.URL, http: ts.Client()}, log: quiet()}
	sum := sha256.Sum256(good)
	offer := &api.AgentUpdate{Version: "2.0.0", Name: "better-backup-agent_2.0.0_linux_amd64.deb", Size: int64(len(good)), SHA256: hex.EncodeToString(sum[:])}
	to := filepath.Join(t.TempDir(), "x.deb")
	if err := a.fetchInstaller(context.Background(), offer, to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(to); !bytes.Equal(b, good) {
		t.Fatal("the installer was not saved whole")
	}
	tampered := append([]byte{}, good...)
	tampered[500] ^= 1
	for name, body := range map[string][]byte{"changed": tampered, "longer": append(append([]byte{}, good...), 'x'), "shorter": good[:100]} {
		served = body
		err := a.fetchInstaller(context.Background(), offer, to)
		if err == nil || !strings.Contains(err.Error(), "not the one the server described") {
			t.Errorf("%s: %v", name, err)
		}
		if _, serr := os.Stat(to); serr == nil {
			t.Errorf("%s: the bad installer was kept", name)
		}
	}
}

func TestCheckAuthenticode(t *testing.T) {
	const us = "CN=better-backup, O=Example"
	for _, c := range []struct {
		out  string
		fail bool
		ok   bool
	}{
		{"NotSigned|\r\nNotSigned|\r\n", false, true},             // nothing is signed yet
		{"Valid|" + us + "\nValid|" + us + "\n", false, true},     // both ours
		{"Valid|" + us + "\nNotSigned|\n", false, false},          // a signed agent takes no unsigned installer
		{"Valid|" + us + "\nHashMismatch|" + us, false, false},    // tampered with
		{"Valid|" + us + "\nValid|CN=someone else", false, false}, // signed, by another
		{"NotSigned|\nValid|" + us, false, true},                  // the first signed version
		{"", true, false},                                         // PowerShell did not run
	} {
		var err error
		if c.fail {
			err = errors.New("exit status 1")
		}
		if got := checkAuthenticode(c.out, err) == nil; got != c.ok {
			t.Errorf("%q: accepted %v, want %v", c.out, got, c.ok)
		}
	}
}
