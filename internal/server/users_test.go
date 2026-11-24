package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// web is a browser's view of the server: no pinned certificate, a
// bearer token once signed in.
type web struct {
	t     *testing.T
	base  string
	token string
	// password, when set, is sent as the password typed again.
	password string
	http     *http.Client
}

func newWeb(t *testing.T, addr string) *web {
	return &web{t: t, base: "https://" + addr, http: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}}
}

// try performs a request and returns the status, or 0 when the server
// could not be reached.
func (w *web) try(method, path string, in, out any) (int, string) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, w.base+path, body)
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	if w.password != "" {
		req.Header.Set("X-BB-Password", w.password)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e api.Error
		json.Unmarshal(data, &e)
		return resp.StatusCode, e.Error
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			w.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
	return resp.StatusCode, ""
}

func (w *web) must(method, path string, in, out any) {
	w.t.Helper()
	if code, msg := w.try(method, path, in, out); code >= 400 || code == 0 {
		w.t.Fatalf("%s %s: %d %s", method, path, code, msg)
	}
}

// refused checks that a request fails with the given status.
func (w *web) refused(want int, method, path string, in any) string {
	w.t.Helper()
	code, msg := w.try(method, path, in, nil)
	if code != want {
		w.t.Fatalf("%s %s: got %d %q, want %d", method, path, code, msg, want)
	}
	return msg
}

func (w *web) as(token string) *web {
	c := *w
	c.token = token
	return &c
}

func (w *web) login(name, password string) *web {
	w.t.Helper()
	var s api.Session
	w.must("POST", "/api/v1/login", api.LoginRequest{Name: name, Password: password}, &s)
	return w.as(s.Token)
}

func startServer(t *testing.T, cfg server.Config) (*server.Server, func()) {
	t.Helper()
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
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return srv, stop
}

// TestUsers covers first-run setup, signing in, what each role may do,
// the activity log, and the settings only an administrator reaches.
func TestUsers(t *testing.T) {
	data := t.TempDir()
	srv, _ := startServer(t, server.Config{DataDir: data, Listen: "127.0.0.1:0", Tick: 50 * time.Millisecond, Log: quiet})
	w := newWeb(t, srv.Addr())
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)

	var st api.SetupStatus
	w.must("GET", "/api/v1/setup", nil, &st)
	if !st.NeedsSetup || st.NeedsToken || !st.CanRecover || st.Locked {
		t.Fatalf("a new server: %+v", st)
	}
	w.refused(401, "GET", "/api/v1/status", nil)
	w.refused(400, "POST", "/api/v1/setup", api.SetupRequest{Name: "andrew", Password: "short"})

	// Once the server has an agent it is in use: the first user then
	// needs the admin token.
	var tok api.EnrollToken
	w.as(af.Token).must("POST", "/api/v1/enroll-tokens", nil, &tok)
	if _, err := agent.Enroll(t.TempDir(), w.base, tok.Token, tok.Fingerprint, "pc"); err != nil {
		t.Fatal(err)
	}
	st = api.SetupStatus{}
	w.must("GET", "/api/v1/setup", nil, &st)
	if !st.NeedsToken || st.CanRecover {
		t.Fatalf("a server with an agent: %+v", st)
	}
	w.refused(403, "POST", "/api/v1/setup", api.SetupRequest{Name: "mallory", Password: "a long password"})
	var sess api.Session
	w.must("POST", "/api/v1/setup", api.SetupRequest{Name: "Andrew", Password: "correct horse battery", Token: af.Token}, &sess)
	if sess.User.Role != api.RoleAdmin || sess.Token == "" {
		t.Fatalf("setup: %+v", sess)
	}
	w.refused(409, "POST", "/api/v1/setup", api.SetupRequest{Name: "second", Password: "another long one", Token: af.Token})
	admin := w.as(sess.Token)

	// Names ignore case; a wrong password and an unknown name look alike.
	w.login("andrew", "correct horse battery")
	if a, b := w.refused(401, "POST", "/api/v1/login", api.LoginRequest{Name: "andrew", Password: "nope"}),
		w.refused(401, "POST", "/api/v1/login", api.LoginRequest{Name: "nobody", Password: "nope"}); a != b {
		t.Fatalf("%q and %q tell a wrong password from a wrong name", a, b)
	}

	var op, viewer api.User
	admin.must("POST", "/api/v1/users", api.User{Name: "olga", Role: api.RoleOperator, Password: "operator password"}, &op)
	admin.must("POST", "/api/v1/users", api.User{Name: "victor", Role: api.RoleViewer, Password: "viewer password"}, &viewer)
	admin.refused(400, "POST", "/api/v1/users", api.User{Name: "OLGA", Role: api.RoleViewer, Password: "operator password"})
	admin.refused(400, "POST", "/api/v1/users", api.User{Name: "root", Role: "owner", Password: "operator password"})

	v := w.login("victor", "viewer password")
	v.must("GET", "/api/v1/status", nil, nil)
	v.must("GET", "/api/v1/snapshots", nil, nil)
	v.refused(403, "POST", "/api/v1/repo/verify", nil)
	v.refused(403, "POST", "/api/v1/enroll-tokens", nil)
	v.refused(403, "GET", "/api/v1/users", nil)
	o := w.login("olga", "operator password")
	o.must("POST", "/api/v1/repo/verify", nil, nil)
	o.refused(403, "GET", "/api/v1/users", nil)
	o.refused(403, "GET", "/api/v1/audit", nil)
	o.refused(403, "PUT", "/api/v1/alerts", api.AlertSettings{})
	o.refused(403, "POST", "/api/v1/settings/repo-key", map[string]string{"password": "operator password"})

	// Changing one's own password ends one's other sign-ins.
	o2 := w.login("olga", "operator password")
	o.refused(403, "PUT", "/api/v1/me/password", map[string]string{"old": "wrong", "new": "a brand new password"})
	o.must("PUT", "/api/v1/me/password", map[string]string{"old": "operator password", "new": "a brand new password"}, nil)
	o.must("GET", "/api/v1/me", nil, nil)
	o2.refused(401, "GET", "/api/v1/me", nil)
	w.login("olga", "a brand new password")

	// The only administrator cannot be removed, demoted or locked.
	me := sess.User
	admin.refused(400, "DELETE", "/api/v1/users/"+me.ID, nil)
	admin.refused(400, "PUT", "/api/v1/users/"+me.ID, api.User{Name: me.Name, Role: api.RoleOperator})
	admin.refused(400, "PUT", "/api/v1/users/"+me.ID, api.User{Name: me.Name, Role: api.RoleAdmin, Disabled: true})
	// A locked user is signed out at once.
	admin.must("PUT", "/api/v1/users/"+viewer.ID, api.User{Name: "victor", Role: api.RoleViewer, Disabled: true}, nil)
	v.refused(401, "GET", "/api/v1/status", nil)
	w.refused(401, "POST", "/api/v1/login", api.LoginRequest{Name: "victor", Password: "viewer password"})
	admin.must("DELETE", "/api/v1/users/"+viewer.ID, nil, nil)

	// The repository key takes the password again, and is the key file.
	admin.refused(403, "POST", "/api/v1/settings/repo-key", map[string]string{"password": "wrong"})
	var status api.Status
	admin.must("GET", "/api/v1/status", nil, &status)
	if status.KeySaved {
		t.Fatal("the key counts as saved before anyone downloaded it")
	}
	var key struct{ Key string }
	admin.must("POST", "/api/v1/settings/repo-key", map[string]string{"password": "correct horse battery"}, &key)
	file, _ := os.ReadFile(filepath.Join(data, "repo.key"))
	if key.Key == "" || key.Key != strings.TrimSpace(string(file)) {
		t.Fatal("the downloaded key is not the key file")
	}
	admin.must("GET", "/api/v1/status", nil, &status)
	if !status.KeySaved {
		t.Fatal("the download was not remembered")
	}

	// Names added in the settings appear in the certificate at once.
	var set api.ServerSettings
	admin.refused(400, "PUT", "/api/v1/settings", api.ServerSettings{TLSNames: []string{"https://backup.example.com:8443"}})
	admin.must("PUT", "/api/v1/settings", api.ServerSettings{TLSNames: []string{"backup.example.com", "192.0.2.7"}}, &set)
	if set.KeySource != "file" || len(set.TLSNames) != 2 {
		t.Fatalf("settings: %+v", set)
	}
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	if !slices.Contains(leaf.DNSNames, "backup.example.com") || len(leaf.IPAddresses) < 3 {
		t.Fatalf("certificate names: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}

	// A new admin token replaces the old one, also in admin.json.
	var nt struct{ Token string }
	admin.must("POST", "/api/v1/settings/admin-token", map[string]string{"password": "correct horse battery"}, &nt)
	w.as(af.Token).refused(401, "GET", "/api/v1/status", nil)
	w.as(nt.Token).must("GET", "/api/v1/status", nil, nil)
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	if af.Token != nt.Token {
		t.Fatal("admin.json does not hold the new token")
	}

	// The activity log names who did what.
	var log []api.AuditEntry
	admin.must("GET", "/api/v1/audit?limit=200", nil, &log)
	has := func(user, action, detail string) bool {
		return slices.ContainsFunc(log, func(e api.AuditEntry) bool {
			return e.User == user && e.Action == action && strings.Contains(e.Detail, detail)
		})
	}
	for _, want := range [][3]string{
		{"Andrew", "Set up the server", ""}, {"Andrew", "Added a user", "olga (operator)"}, {"olga", "Started verifying all data", ""},
		{"olga", "Changed their own password", ""}, {"Andrew", "Changed a user", "sign-in turned off"}, {"Andrew", "Removed a user", "victor"},
		{"Andrew", "Downloaded the repository key", ""}, {"Andrew", "Changed the server's names", "backup.example.com"},
		{"nobody", "Failed to sign in", ""}, {"admin token", "Created an enrollment token for a new agent", ""},
	} {
		if !has(want[0], want[1], want[2]) {
			t.Errorf("the activity log lacks %q", want)
		}
	}
	if has("olga", "Changed the email alert settings", "") {
		t.Error("a refused change was logged as made")
	}

	// Signing out ends the session.
	o.must("POST", "/api/v1/logout", nil, nil)
	o.refused(401, "GET", "/api/v1/me", nil)

	// Guessing is slowed down: after five wrong tries in a row, one of
	// them made above, the next has to wait.
	for i := 0; i < 4; i++ {
		w.refused(401, "POST", "/api/v1/login", api.LoginRequest{Name: "andrew", Password: "guess"})
	}
	if msg := w.refused(429, "POST", "/api/v1/login", api.LoginRequest{Name: "andrew", Password: "correct horse battery"}); !strings.Contains(msg, "Try again") {
		t.Fatal(msg)
	}
	admin.must("GET", "/api/v1/status", nil, nil) // the session in use is not affected
}

// TestRecoverServer loses a server twice. First everything is gone and
// a new server is rebuilt from the offsite copy; then only the data
// directory is gone and a new server is given the key of the repository
// it finds. Both times the users, the policies and the agent's trust
// come back, and the agent carries on without being enrolled again.
func TestRecoverServer(t *testing.T) {
	offsiteDir := t.TempDir()
	cfgA := server.Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Tick: 50 * time.Millisecond, Log: quiet}
	a, stopA := startServer(t, cfgA)
	addr := a.Addr()
	w := newWeb(t, addr)
	var sess api.Session
	w.must("POST", "/api/v1/setup", api.SetupRequest{Name: "andrew", Password: "correct horse battery"}, &sess)
	admin := w.as(sess.Token)

	var tok api.EnrollToken
	admin.must("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, w.base, tok.Token, tok.Fingerprint, "file-server"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(adir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { stopAgent(); <-agentDone }()

	src := t.TempDir()
	write(t, filepath.Join(src, "report.txt"), rnd(1, 300_000))
	var agents []api.Agent
	admin.must("GET", "/api/v1/agents", nil, &agents)
	var pol api.Policy
	admin.must("POST", "/api/v1/policies", api.Policy{Name: "documents", AgentID: agents[0].ID, Paths: []string{src}, Enabled: true}, &pol)
	backup := func(c *web) {
		t.Helper()
		var j api.Job
		c.must("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j)
		waitJob(t, c, j.ID)
	}
	backup(admin)
	admin.must("PUT", "/api/v1/offsite", api.OffsiteSettings{Enabled: true, Kind: "folder", Folder: offsiteDir}, nil)
	var j api.Job
	admin.must("POST", "/api/v1/offsite/run", nil, &j)
	waitJob(t, admin, j.ID)
	var key struct{ Key string }
	admin.must("POST", "/api/v1/settings/repo-key", map[string]string{"password": "correct horse battery"}, &key)
	stopA()

	// A new machine: empty data directory, empty repository, same address.
	repoB := t.TempDir()
	cfgB := server.Config{DataDir: t.TempDir(), RepoDir: repoB, Listen: addr, Tick: 50 * time.Millisecond, Log: quiet}
	stopB := runServer(t, cfgB)
	var st api.SetupStatus
	waitFor(t, "the new server", func() bool { code, _ := w.try("GET", "/api/v1/setup", nil, &st); return code == 200 })
	if !st.NeedsSetup || !st.CanRecover {
		t.Fatalf("new server: %+v", st)
	}
	from := api.OffsiteSettings{Kind: "folder", Folder: offsiteDir}
	if msg := w.refused(400, "POST", "/api/v1/setup/recover", api.RecoverRequest{Offsite: from, Key: "not the key"}); !strings.Contains(msg, "not the key of the copy") {
		t.Fatal(msg)
	}
	w.refused(502, "POST", "/api/v1/setup/recover", api.RecoverRequest{Offsite: api.OffsiteSettings{Kind: "folder", Folder: t.TempDir()}, Key: key.Key})
	st = api.SetupStatus{}
	w.must("POST", "/api/v1/setup/recover", api.RecoverRequest{Offsite: from, Key: key.Key + "\n"}, &st)
	if st.Recovery == nil {
		t.Fatalf("no progress reported: %+v", st)
	}
	waitFor(t, "the recovered server", func() bool {
		var now api.SetupStatus
		code, _ := w.try("GET", "/api/v1/setup", nil, &now)
		if now.Recovery != nil && now.Recovery.Error != "" {
			t.Fatal(now.Recovery.Error)
		}
		return code == 200 && !now.NeedsSetup && now.Recovery == nil
	})
	check := func(what string) *web {
		t.Helper()
		c := w.login("andrew", "correct horse battery")
		var snaps []json.RawMessage
		c.must("GET", "/api/v1/snapshots", nil, &snaps)
		var pols []api.Policy
		c.must("GET", "/api/v1/policies", nil, &pols)
		if len(snaps) == 0 || len(pols) != 1 || pols[0].Name != "documents" {
			t.Fatalf("%s: %d snapshots, policies %+v", what, len(snaps), pols)
		}
		// The agent still trusts the server and is still known to it.
		write(t, filepath.Join(src, what+".txt"), rnd(2, 50_000))
		backup(c)
		return c
	}
	b := check("after the offsite copy")
	var set api.ServerSettings
	b.must("GET", "/api/v1/settings", nil, &set)
	if set.StateSaved == nil {
		t.Fatal("the recovered server never saved its own configuration")
	}
	waitFor(t, "the configuration saved after the last backup", func() bool {
		// Saved daily; ask for it now through an offsite run.
		var j api.Job
		b.must("POST", "/api/v1/offsite/run", nil, &j)
		return waitJob(t, b, j.ID).Status == api.StatusSuccess
	})
	stopB()

	// The repository disk survives, the rest of the server does not.
	cfgC := server.Config{DataDir: t.TempDir(), RepoDir: repoB, Listen: addr, Tick: 50 * time.Millisecond, Log: quiet}
	runServer(t, cfgC)
	waitFor(t, "the locked server", func() bool {
		st = api.SetupStatus{}
		code, _ := w.try("GET", "/api/v1/setup", nil, &st)
		return code == 200
	})
	if !st.Locked || st.LockedNeedsSignIn || st.CanRecover {
		t.Fatalf("server without its key: %+v", st)
	}
	w.refused(503, "GET", "/api/v1/status", nil)
	w.refused(503, "POST", "/api/v1/setup/recover", api.RecoverRequest{Offsite: from, Key: key.Key})
	w.refused(400, "POST", "/api/v1/setup/unlock", api.UnlockRequest{Key: "wrong"})
	w.must("POST", "/api/v1/setup/unlock", api.UnlockRequest{Key: key.Key}, nil)
	waitFor(t, "the unlocked server", func() bool {
		var now api.SetupStatus
		code, _ := w.try("GET", "/api/v1/setup", nil, &now)
		return code == 200 && !now.Locked && !now.NeedsSetup
	})
	check("after the key")
	if b, _ := os.ReadFile(filepath.Join(cfgC.DataDir, "repo.key")); strings.TrimSpace(string(b)) != key.Key {
		t.Fatal("the key file was not written")
	}
}

// runServer runs a server the way bb-server does, restarts included.
func runServer(t *testing.T, cfg server.Config) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, cfg) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server: %v", err)
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitJob(t *testing.T, c *web, id string) *api.Job {
	t.Helper()
	var j api.Job
	waitFor(t, "job "+id, func() bool {
		c.must("GET", "/api/v1/jobs/"+id, nil, &j)
		return j.Status != api.StatusQueued && j.Status != api.StatusRunning
	})
	if j.Status != api.StatusSuccess {
		t.Fatalf("job %s: %s %s", id, j.Status, j.Error)
	}
	return &j
}
