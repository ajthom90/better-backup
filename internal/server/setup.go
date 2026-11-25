package server

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/offsite"
	"github.com/ajthom90/better-backup/internal/repo"
)

// errKeyMissing: the repository is there but nothing says how to open
// it. The server then starts locked and asks for the key in the web UI.
var errKeyMissing = errors.New("the repository key is missing")

// The server's own state, saved into the repository so that an offsite
// copy can bring back a whole server: users, agents, policies, settings
// and the certificate authority agents trust.
const (
	stateFile  = "state/server.bak"
	stateMagic = "BBSTATE1\n"
	stateEvery = 24 * time.Hour
	// recoverDir, inside the repository folder, receives an offsite copy
	// before it takes the place of the empty repository.
	recoverDir = ".recover"
)

var stateContext = []byte("better-backup/server-state/v1")

// saveState writes the database and the certificate authority, sealed
// with the repository's key, to state/server.bak in the repository.
func (s *Server) saveState() error {
	tmp := filepath.Join(s.cfg.DataDir, "state.tmp-"+keys.RandomHex(4))
	defer os.Remove(tmp)
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, path := range map[string]string{"server.db": tmp,
		"tls/ca.crt": filepath.Join(s.cfg.DataDir, "tls", "ca.crt"), "tls/ca.key": filepath.Join(s.cfg.DataDir, "tls", "ca.key")} {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return err
		}
		tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	k := s.repo.Keys()
	out := append([]byte(stateMagic), keys.Seal(&k.Enc, stateContext, buf.Bytes())...)
	dest := filepath.Join(s.cfg.RepoDir, filepath.FromSlash(stateFile))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	part := dest + ".tmp-" + keys.RandomHex(4)
	if err := os.WriteFile(part, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return err
	}
	return s.setSetting("state_saved", time.Now().UTC().Format(time.RFC3339))
}

// maybeSaveState saves the state once a day, and within a minute or so
// of a change someone made.
func (s *Server) maybeSaveState() {
	if t := settingTime(s.setting("state_saved")); t != nil {
		age := time.Since(*t)
		if age < time.Minute || (age < stateEvery && !s.stateDirty.Load()) {
			if _, err := os.Stat(filepath.Join(s.cfg.RepoDir, filepath.FromSlash(stateFile))); err == nil {
				return
			}
		}
	}
	s.stateDirty.Store(false)
	if err := s.saveState(); err != nil {
		s.log.Error("saving the server's configuration into the repository", "err", err)
	}
}

// restoreState puts the state saved in a repository into a data
// directory. It reports false when the repository holds none. The
// server must not be running on that directory.
func restoreState(dataDir, repoDir string, pass []byte) (bool, error) {
	b, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(stateFile)))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cfg, err := os.ReadFile(filepath.Join(repoDir, "config.json"))
	if err != nil {
		return false, err
	}
	k, err := repo.KeysFromConfig(cfg, pass)
	if err != nil {
		return false, err
	}
	if !bytes.HasPrefix(b, []byte(stateMagic)) {
		return false, errors.New("the saved server configuration is in a format this version does not know")
	}
	plain, err := keys.Open(&k.Enc, stateContext, b[len(stateMagic):])
	if err != nil {
		return false, errors.New("the saved server configuration is damaged")
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(plain))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		if files[h.Name], err = io.ReadAll(tr); err != nil {
			return false, err
		}
	}
	for _, name := range []string{"server.db", "tls/ca.crt", "tls/ca.key"} {
		if len(files[name]) == 0 {
			return false, fmt.Errorf("the saved server configuration lacks %s", name)
		}
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "tls"), 0o700); err != nil {
		return false, err
	}
	for _, old := range []string{"server.db-wal", "server.db-shm"} {
		os.Remove(filepath.Join(dataDir, old))
	}
	for name, data := range files {
		mode := os.FileMode(0o600)
		if name == "tls/ca.crt" {
			mode = 0o644
		}
		if err := os.WriteFile(filepath.Join(dataDir, filepath.FromSlash(name)), data, mode); err != nil {
			return false, err
		}
	}
	return true, nil
}

// requestRestart makes Serve return, and Run call then and start the
// server again. then runs while nothing has the data directory or the
// repository open.
func (s *Server) requestRestart(then func() error) {
	s.mu.Lock()
	s.restart = then
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		// Let the answer to the request that asked for this go out.
		time.AfterFunc(300*time.Millisecond, cancel)
	}
}

// Run serves until ctx is cancelled, starting the server again whenever
// it asks to be: after a recovery from an offsite copy, or after it was
// given its repository key.
func Run(ctx context.Context, cfg Config) error {
	for {
		s, err := New(cfg)
		if err != nil {
			return err
		}
		err = s.Serve(ctx)
		s.Close()
		s.mu.Lock()
		then := s.restart
		s.mu.Unlock()
		if err != nil || then == nil || ctx.Err() != nil {
			return err
		}
		if err := then(); err != nil {
			s.log.Error("the step before the restart failed", "err", err)
		}
		s.log.Info("starting again")
	}
}

func (s *Server) agentCount() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n)
	return n
}

// pristine reports whether this server has never been used: nothing
// would be lost if an offsite copy took its place.
func (s *Server) pristine() bool {
	if s.locked || s.userCount() > 0 || s.agentCount() > 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snaps) == 0
}

func (s *Server) setupStatus() api.SetupStatus {
	st := api.SetupStatus{Version: Version, Locked: s.locked, KeyFromEnv: s.cfg.Passphrase != ""}
	users, agents := s.userCount(), s.agentCount()
	st.NeedsSetup = users == 0
	st.NeedsToken = users == 0 && agents > 0
	st.LockedNeedsSignIn = s.locked && (users > 0 || agents > 0)
	st.CanRecover = s.pristine()
	s.mu.Lock()
	if s.recover != nil {
		p := *s.recover
		st.Recovery = &p
	}
	s.mu.Unlock()
	return st
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.setupStatus())
}

// handleSetup creates the first administrator. On a server that already
// has agents it needs the admin token, so that an upgrade does not leave
// the server open to whoever finds its page first.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req api.SetupRequest
	if !decode(w, r, &req) {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.userCount() > 0 {
		fail(w, http.StatusConflict, "This server is already set up. Sign in instead.")
		return
	}
	if s.agentCount() > 0 && subtle.ConstantTimeCompare([]byte(hashToken(strings.TrimSpace(req.Token))), []byte(s.setting("admin_token_hash"))) != 1 {
		fail(w, http.StatusForbidden, "The admin token is wrong. It is in admin.json in the server's data directory.")
		return
	}
	u, err := s.createUser(req.Name, req.Password, api.RoleAdmin)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.newSession(u)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.audit(u.Name, remoteHost(r), "Set up the server", "created the first administrator")
	reply(w, http.StatusCreated, sess)
}

// handleUnlock takes the key of a repository the server found without
// one, for example on a disk moved over from a lost server.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var req api.UnlockRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.locked {
		fail(w, http.StatusConflict, "This server has its repository key.")
		return
	}
	fresh := s.userCount() == 0 && s.agentCount() == 0
	user := "setup"
	if !fresh {
		p := s.authAdmin(r)
		if p == nil || p.role != api.RoleAdmin {
			fail(w, http.StatusUnauthorized, "Sign in as an administrator to enter the repository key.")
			return
		}
		user = p.name
	}
	key := []byte(strings.TrimSpace(req.Key))
	cfg, err := os.ReadFile(filepath.Join(s.cfg.RepoDir, "config.json"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if _, err := repo.KeysFromConfig(cfg, key); err != nil {
		fail(w, http.StatusBadRequest, "This is not the key of the repository in "+s.cfg.RepoDir+".")
		return
	}
	s.audit(user, remoteHost(r), "Entered the repository key", "")
	data, repoDir, log := s.cfg.DataDir, s.cfg.RepoDir, s.log
	s.requestRestart(func() error {
		if err := os.WriteFile(filepath.Join(data, "repo.key"), append(key, '\n'), 0o600); err != nil {
			return err
		}
		if !fresh {
			return nil
		}
		// A new server on an old repository takes over the old server's
		// users, agents and policies when the repository holds them.
		ok, err := restoreState(data, repoDir, key)
		if ok {
			log.Info("restored the server's configuration from the repository")
		}
		return err
	})
	reply(w, http.StatusAccepted, map[string]bool{"restarting": true, "restores_configuration": fresh && fileExists(filepath.Join(repoDir, filepath.FromSlash(stateFile)))})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// handleRecover replaces a new, empty server with what an offsite copy
// holds. It answers at once; the download is followed through the setup
// status.
func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	var req api.RecoverRequest
	if !decode(w, r, &req) {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.pristine() {
		fail(w, http.StatusConflict, "Only a new server, without users, agents or backups, can be replaced by an offsite copy.")
		return
	}
	s.mu.Lock()
	busy := s.recover != nil && (s.recover.Running || s.recover.Restarting)
	s.mu.Unlock()
	if busy {
		fail(w, http.StatusConflict, "A recovery is already running.")
		return
	}
	key := []byte(strings.TrimSpace(req.Key))
	if len(key) == 0 {
		fail(w, http.StatusBadRequest, "Enter the repository key: the contents of repo.key of the old server.")
		return
	}
	if s.cfg.Passphrase != "" && s.cfg.Passphrase != string(key) {
		fail(w, http.StatusBadRequest, "This server gets its repository key from BB_REPO_PASSPHRASE, and that is a different key. Set it to the old server's key, or remove it, and start the server again.")
		return
	}
	req.Offsite.Enabled = true
	if err := checkOffsiteSettings(&req.Offsite); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := offsiteTarget(req.Offsite)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AsOf != nil {
		l, ok := t.(offsite.Locker)
		if !ok {
			fail(w, http.StatusBadRequest, "Only a copy in a bucket with Object Lock can be taken as it was at an earlier time.")
			return
		}
		if req.AsOf.After(time.Now()) {
			fail(w, http.StatusBadRequest, "The time to take the copy from is in the future.")
			return
		}
		t = l.AsOf(*req.AsOf)
	}
	// The key is checked against the copy before the long download.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	rc, err := t.Get(ctx, "config.json")
	if err != nil {
		fail(w, http.StatusBadGateway, fmt.Sprintf("%s holds no complete copy of a repository, or cannot be reached: %v", t, err))
		return
	}
	cfg, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	rc.Close()
	if err != nil {
		fail(w, http.StatusBadGateway, "reading the copy: "+err.Error())
		return
	}
	if _, err := repo.KeysFromConfig(cfg, key); err != nil {
		fail(w, http.StatusBadRequest, "This is not the key of the copy in "+t.String()+".")
		return
	}
	s.mu.Lock()
	s.recover = &api.RecoveryProgress{Running: true}
	s.mu.Unlock()
	s.audit("setup", remoteHost(r), "Started recovering the server from the offsite copy", t.String())
	go s.runRecover(t, key)
	reply(w, http.StatusAccepted, s.setupStatus())
}

func (s *Server) runRecover(t offsite.Target, key []byte) {
	tmp := filepath.Join(s.cfg.RepoDir, recoverDir)
	set := func(f func(p *api.RecoveryProgress)) {
		s.mu.Lock()
		f(s.recover)
		s.mu.Unlock()
	}
	err := offsite.Fetch(s.ctx, t, tmp, func(done, total int64) {
		set(func(p *api.RecoveryProgress) { p.Done, p.Total = done, total })
	})
	if err == nil && !s.pristine() {
		err = errors.New("the server was put to use while the copy was downloading; nothing was replaced")
	}
	if err != nil {
		s.log.Error("recovering from the offsite copy", "err", err)
		set(func(p *api.RecoveryProgress) {
			p.Running = false
			p.Error = err.Error() + ". Start the recovery again to continue where it stopped."
		})
		return
	}
	set(func(p *api.RecoveryProgress) { p.Running, p.Restarting = false, true })
	data, repoDir, fromEnv, log := s.cfg.DataDir, s.cfg.RepoDir, s.cfg.Passphrase != "", s.log
	s.requestRestart(func() error {
		if err := adoptRepo(repoDir, tmp); err != nil {
			return err
		}
		if !fromEnv {
			if err := os.WriteFile(filepath.Join(data, "repo.key"), append(key, '\n'), 0o600); err != nil {
				return err
			}
		}
		ok, err := restoreState(data, repoDir, key)
		if ok {
			log.Info("restored the server's configuration from the offsite copy")
		} else if err == nil {
			log.Warn("the offsite copy holds no server configuration: users, agents and policies must be set up again")
		}
		return err
	})
}

// adoptRepo moves a downloaded repository from tmp into dir, which
// holds the empty repository of a new server. It works on what is in
// dir rather than on dir itself, which may be a mount point.
func adoptRepo(dir, tmp string) error {
	if packs, _ := filepath.Glob(filepath.Join(dir, "packs", "*", "*.pack")); len(packs) > 0 {
		return fmt.Errorf("%s already holds backups; it was left alone", dir)
	}
	for _, n := range []string{"config.json", "index.db", "index.db-wal", "index.db-shm", "packs", "snapshots", "tmp", "state"} {
		if err := os.RemoveAll(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(tmp)
}
