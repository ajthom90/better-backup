// Package server implements the better-backup server: it owns the
// repository, hands jobs to agents, enforces schedules and retention, and
// serves the admin API and web UI.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/tree"
)

// Version is set at build time.
var Version = "dev"

//go:embed ui/index.html
var uiFS embed.FS

// Config configures a server.
type Config struct {
	DataDir string // server state: database, CA, admin credentials
	RepoDir string // repository; default DataDir/repo
	Listen  string // default ":8443"
	// TLSNames are extra DNS names or IPs for the server certificate.
	TLSNames []string
	// Passphrase protects the repository master key. If empty, a random
	// key is generated on first start and kept in DataDir/repo.key.
	Passphrase string
	Log        *slog.Logger

	// Tick is how often schedules and timeouts are checked.
	Tick time.Duration
}

// AdminFile is written to the data directory so the CLI on the same
// machine can reach the server without configuration.
type AdminFile struct {
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

// Server is a running better-backup server.
type Server struct {
	cfg  Config
	log  *slog.Logger
	db   *sql.DB
	repo *repo.Repo
	ca   *authority
	fp   string
	enc  *zstd.Encoder
	dec  *zstd.Decoder
	ln   net.Listener
	http *http.Server

	mu      sync.Mutex
	snaps   map[string]*tree.Snapshot
	running map[string]string // agent id -> its running job
	maint   bool              // GC or verify in progress: no agent jobs start
	// offsiteBusy is set while the offsite copy is being updated;
	// garbage collection waits for it.
	offsiteBusy bool
	wake        map[string]chan struct{}
	kick        chan struct{}
	stop        chan struct{} // closed at shutdown to release long polls
	mail        chan mailMsg  // alerts waiting to be sent
}

// New opens, or on first start creates, the server state and repository.
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = ":8443"
	}
	if cfg.RepoDir == "" {
		cfg.RepoDir = filepath.Join(cfg.DataDir, "repo")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Tick == 0 {
		cfg.Tick = 20 * time.Second
	}
	s := &Server{cfg: cfg, log: cfg.Log, snaps: map[string]*tree.Snapshot{}, running: map[string]string{},
		wake: map[string]chan struct{}{}, kick: make(chan struct{}, 1), stop: make(chan struct{}), mail: make(chan mailMsg, 64)}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	pass, err := s.passphrase()
	if err != nil {
		return nil, err
	}
	if !repo.Exists(cfg.RepoDir) {
		if err := repo.Init(cfg.RepoDir, pass); err != nil {
			return nil, err
		}
		s.log.Info("created a new repository", "dir", cfg.RepoDir)
	}
	if s.repo, err = repo.Open(cfg.RepoDir, pass); err != nil {
		return nil, err
	}
	if s.db, err = openDB(cfg.DataDir); err != nil {
		s.repo.Close()
		return nil, err
	}
	if s.ca, err = loadOrCreateCA(filepath.Join(cfg.DataDir, "tls")); err != nil {
		s.Close()
		return nil, err
	}
	s.fp, _ = api.Fingerprint(s.ca.certPEM)
	s.enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	s.dec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(repo.MaxChunkSize))

	snaps, err := s.repo.LoadSnapshots()
	if err != nil {
		s.Close()
		return nil, err
	}
	for _, sn := range snaps {
		s.snaps[sn.ID] = sn
	}
	// Jobs that were running when the server last stopped cannot finish.
	if _, err := s.db.Exec(`UPDATE jobs SET status = ?, error = 'the server restarted while this job was running', finished = ? WHERE status = ?`,
		api.StatusFailed, ms(time.Now()), api.StatusRunning); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// passphrase returns the repository passphrase, creating a key file on
// first start when none was configured.
func (s *Server) passphrase() ([]byte, error) {
	if s.cfg.Passphrase != "" {
		return []byte(s.cfg.Passphrase), nil
	}
	path := filepath.Join(s.cfg.DataDir, "repo.key")
	b, err := os.ReadFile(path)
	if err == nil {
		return []byte(strings.TrimSpace(string(b))), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if repo.Exists(s.cfg.RepoDir) {
		return nil, fmt.Errorf("the repository in %s exists but its key file %s is missing; restore the key file or set BB_REPO_PASSPHRASE", s.cfg.RepoDir, path)
	}
	key := keys.RandomHex(32)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return nil, err
	}
	s.log.Warn("generated a repository key. Copy this file somewhere safe: without it the backups cannot be read", "file", path)
	return []byte(key), nil
}

// Close releases the database and repository.
func (s *Server) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.repo != nil {
		if rerr := s.repo.Close(); err == nil {
			err = rerr
		}
	}
	return err
}

// Fingerprint returns the CA pin agents and the CLI use.
func (s *Server) Fingerprint() string { return s.fp }

// Listen binds the server's port. Addr is valid afterwards.
func (s *Server) Listen() error {
	cert, err := s.ca.serverCert(s.cfg.TLSNames)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.cert)
	ln, err := tls.Listen("tcp", s.cfg.Listen, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		// Agents present a certificate; browsers and the CLI do not.
		ClientAuth: tls.VerifyClientCertIfGiven,
	})
	if err != nil {
		return err
	}
	s.ln = ln
	return s.writeAdminFile()
}

// Addr returns the listening address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) localURL() string {
	_, port, _ := net.SplitHostPort(s.Addr())
	return "https://localhost:" + port
}

// writeAdminFile creates the admin token on first start and records how
// to reach this server for the local CLI.
func (s *Server) writeAdminFile() error {
	path := filepath.Join(s.cfg.DataDir, "admin.json")
	var af AdminFile
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &af)
	}
	if af.Token == "" || s.setting("admin_token_hash") != hashToken(af.Token) {
		af.Token = keys.RandomHex(24)
		if err := s.setSetting("admin_token_hash", hashToken(af.Token)); err != nil {
			return err
		}
		s.log.Info("created the admin token", "file", path)
	}
	af.URL, af.Fingerprint = s.localURL(), s.fp
	b, _ := json.MarshalIndent(af, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	s.http = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 30 * time.Second,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug)}
	bg, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.background(bg) }()
	go s.mailer(s.stop)
	go func() {
		<-ctx.Done()
		close(s.stop)
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		s.http.Shutdown(sctx)
	}()
	s.log.Info("better-backup server listening", "addr", s.Addr(), "version", Version, "fingerprint", s.fp)
	err := s.http.Serve(s.ln)
	cancel()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.Write(b)
	})
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(s.ca.certPEM)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	mux.HandleFunc("POST /agent/v1/enroll", s.handleEnroll)
	ag := func(pattern string, h func(http.ResponseWriter, *http.Request, *api.Agent)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			a, err := s.authAgent(r)
			if err != nil {
				fail(w, http.StatusUnauthorized, err.Error())
				return
			}
			h(w, r, a)
		})
	}
	ag("GET /agent/v1/jobs/next", s.handleNextJob)
	ag("POST /agent/v1/jobs/{id}/progress", s.handleProgress)
	ag("POST /agent/v1/jobs/{id}/complete", s.handleComplete)
	ag("POST /agent/v1/chunks/missing", s.handleMissing)
	ag("PUT /agent/v1/chunks/{id}", s.handlePutChunk)
	ag("GET /agent/v1/chunks/{id}", s.handleGetChunk)
	ag("GET /agent/v1/summary", s.handleAgentSummary)
	ag("POST /agent/v1/backup", s.handleAgentBackup)

	ad := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(s.setting("admin_token_hash"))) != 1 {
				fail(w, http.StatusUnauthorized, "invalid or missing admin token")
				return
			}
			h(w, r)
		})
	}
	ad("GET /api/v1/status", s.handleStatus)
	ad("POST /api/v1/enroll-tokens", s.handleCreateToken)
	ad("GET /api/v1/agents", s.handleAgents)
	ad("DELETE /api/v1/agents/{id}", s.handleRevokeAgent)
	ad("GET /api/v1/policies", s.handlePolicies)
	ad("POST /api/v1/policies", s.handleSavePolicy)
	ad("PUT /api/v1/policies/{id}", s.handleSavePolicy)
	ad("DELETE /api/v1/policies/{id}", s.handleDeletePolicy)
	ad("POST /api/v1/policies/{id}/run", s.handleRunPolicy)
	ad("GET /api/v1/alerts", s.handleAlertSettings)
	ad("PUT /api/v1/alerts", s.handleSaveAlertSettings)
	ad("POST /api/v1/alerts/test", s.handleTestAlert)
	ad("GET /api/v1/jobs", s.handleJobs)
	ad("GET /api/v1/jobs/{id}", s.handleJob)
	ad("GET /api/v1/snapshots", s.handleSnapshots)
	ad("GET /api/v1/snapshots/{id}", s.handleSnapshot)
	ad("DELETE /api/v1/snapshots/{id}", s.handleDeleteSnapshot)
	ad("GET /api/v1/snapshots/{id}/ls", s.handleList)
	ad("POST /api/v1/restores", s.handleRestore)
	ad("GET /api/v1/offsite", s.handleOffsite)
	ad("PUT /api/v1/offsite", s.handleSaveOffsite)
	ad("POST /api/v1/offsite/test", s.handleTestOffsite)
	ad("POST /api/v1/offsite/run", s.handleRunOffsite)
	ad("POST /api/v1/repo/gc", s.handleMaintenance(api.JobGC))
	ad("POST /api/v1/repo/verify", s.handleMaintenance(api.JobVerify))
	return mux
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { reply(w, code, api.Error{Error: msg}) }

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	fail(w, http.StatusInternalServerError, err.Error())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// wakeAgent nudges an agent's long poll.
func (s *Server) wakeAgent(id string) {
	s.mu.Lock()
	ch := s.wake[id]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) wakeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.wake {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// kickBackground makes the background loop run now.
func (s *Server) kickBackground() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Server) enqueue(j *api.Job) error {
	j.ID, j.Status, j.Created = keys.RandomHex(8), api.StatusQueued, time.Now()
	var spec []byte
	switch {
	case j.Backup != nil:
		spec, _ = json.Marshal(j.Backup)
	case j.Restore != nil:
		spec, _ = json.Marshal(j.Restore)
	}
	_, err := s.db.Exec(`INSERT INTO jobs (id, type, status, agent_id, policy_id, snapshot_id, spec, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Type, j.Status, j.AgentID, j.PolicyID, j.SnapshotID, string(spec), ms(j.Created))
	if err != nil {
		return err
	}
	if j.AgentID != "" {
		s.wakeAgent(j.AgentID)
	} else {
		s.kickBackground()
	}
	return nil
}

// finish records a job's outcome and frees its agent.
func (s *Server) finish(j *api.Job, status, errMsg string, result any, warnings []string) error {
	var res, warn []byte
	if result != nil {
		res, _ = json.Marshal(result)
	}
	if len(warnings) > 0 {
		if len(warnings) > api.MaxWarnings {
			warnings = warnings[:api.MaxWarnings]
		}
		warn, _ = json.Marshal(warnings)
	}
	_, err := s.db.Exec(`UPDATE jobs SET status = ?, error = ?, result = ?, warnings = ?, snapshot_id = ?, finished = ? WHERE id = ?`,
		status, errMsg, string(res), string(warn), j.SnapshotID, ms(time.Now()), j.ID)
	s.mu.Lock()
	if j.AgentID != "" && s.running[j.AgentID] == j.ID {
		delete(s.running, j.AgentID)
	}
	s.mu.Unlock()
	s.kickBackground()
	s.alertJob(j, status, errMsg, warnings)
	s.log.Info("job finished", "job", j.ID, "type", j.Type, "status", status, "error", errMsg)
	return err
}
