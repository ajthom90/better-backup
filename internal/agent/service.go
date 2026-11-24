package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ajthom90/better-backup/internal/agentctl"
	"github.com/ajthom90/better-backup/internal/api"
)

// Service is the agent as it is installed: a program that always runs.
// Until the machine is enrolled it waits to be; once it is, it runs the
// agent. All the while it answers the local control channel, which is
// how the bb-agent commands and the desktop app enroll the machine, see
// what the agent is doing and start a backup.
type Service struct {
	dir    string
	socket string
	log    *slog.Logger

	// enrollMu serializes enrolling and unenrolling.
	enrollMu sync.Mutex

	mu      sync.Mutex
	agent   *Agent             // the running agent, when enrolled
	stop    context.CancelFunc // stops it
	stopped chan struct{}      // closed when it has stopped
	err     string             // why the agent is not running or not enrolled
	changed chan struct{}      // nudges the run loop

	sumMu   sync.Mutex
	summary *api.AgentSummary
	sumAt   time.Time
}

// NewService returns the service for the agent state in dir.
func NewService(dir string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{dir: dir, socket: agentctl.SocketPath(dir), log: log, changed: make(chan struct{}, 1)}
}

// Socket is where the service's control channel is.
func (s *Service) Socket() string { return s.socket }

func (s *Service) nudge() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Service) setErr(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.err = msg
	s.mu.Unlock()
}

// Run runs until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The agent is useful without its control channel (in a
		// container, say), so failing to open it is not fatal.
		if err := agentctl.Serve(ctx, s.socket, s); err != nil && ctx.Err() == nil {
			s.log.Warn("the local control channel is not available; bb-agent commands will work on the agent's files directly", "err", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.watchFullDiskAccess(ctx)
	}()

	wait := func(d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-s.changed:
		case <-t.C:
		}
	}
	announced := false
	retry := 3 * time.Second
	for ctx.Err() == nil {
		if !Enrolled(s.dir) {
			if !announced {
				s.log.Info("this machine is not enrolled yet; waiting. Enroll it with: bb-agent enroll", "version", Version)
				announced = true
			}
			// A code left by the installer enrolls the machine without
			// anyone at the keyboard.
			if again := s.preseed(); again {
				wait(retry)
				retry = min(retry*2, time.Minute)
			} else {
				retry = 3 * time.Second
				// Also notice an enrollment made by writing the files.
				wait(3 * time.Second)
			}
			continue
		}
		announced = false
		a, err := New(s.dir, s.log)
		if err != nil {
			s.log.Error("cannot start the agent", "err", err)
			s.setErr(err)
			wait(30 * time.Second)
			continue
		}
		actx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		s.mu.Lock()
		s.agent, s.stop, s.stopped, s.err = a, cancel, stopped, ""
		s.mu.Unlock()
		err = a.Run(actx)
		cancel()
		s.mu.Lock()
		s.agent, s.stop, s.stopped = nil, nil, nil
		s.mu.Unlock()
		close(stopped)
		if err != nil {
			return err
		}
	}
	return nil
}

// PreseedFile is the name of a file in the agent's state directory that
// enrolls the machine when the agent starts: installers write it so that
// a silent install needs no second step. Its lines are "code=..." and,
// optionally, "name=...".
const PreseedFile = "enroll.conf"

// preseed enrolls from the preseed file, if there is one. It reports
// whether to try again soon: the server could not be reached.
func (s *Service) preseed() (again bool) {
	path := filepath.Join(s.dir, PreseedFile)
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	var req agentctl.EnrollRequest
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "code":
			req.Code = v
		case "name":
			req.Name = v
		case "server":
			req.Server = v
		case "token":
			req.Token = v
		case "fingerprint":
			req.Fingerprint = v
		}
	}
	f.Close()
	err = s.Enroll(context.Background(), req)
	var perm permanent
	switch {
	case err == nil:
		s.log.Info("enrolled with the code the installer left", "file", path)
	case errors.As(err, &perm):
		s.log.Error("the enrollment code the installer left was refused; enroll with: bb-agent enroll", "err", err)
		s.setErr(fmt.Errorf("the enrollment code given to the installer was refused: %w", err))
	default:
		s.log.Warn("could not enroll with the code the installer left; will try again", "err", err)
		s.setErr(fmt.Errorf("enrolling with the code given to the installer: %w", err))
		return true
	}
	os.Remove(path)
	return false
}

// Enroll enrolls the machine; the agent starts at once.
func (s *Service) Enroll(ctx context.Context, req agentctl.EnrollRequest) error {
	s.enrollMu.Lock()
	defer s.enrollMu.Unlock()
	if _, err := EnrollWith(s.dir, req); err != nil {
		return err
	}
	s.setErr(nil)
	s.nudge()
	return nil
}

// EnrollWith enrolls the machine whose agent state is in dir.
func EnrollWith(dir string, req agentctl.EnrollRequest) (*Config, error) {
	if Enrolled(dir) {
		return nil, permanent{errors.New("this machine is already enrolled; remove it first with: bb-agent unenroll")}
	}
	server, token, fp := req.Server, req.Token, req.Fingerprint
	if strings.TrimSpace(req.Code) != "" {
		var err error
		if server, token, fp, err = api.DecodeEnrollCode(req.Code); err != nil {
			return nil, permanent{err}
		}
	}
	if server == "" || token == "" || fp == "" {
		return nil, permanent{errors.New("enrolling needs the enrollment code from the server's \"Add an agent\" dialog")}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name, _ = os.Hostname()
	}
	return Enroll(dir, server, token, fp, name)
}

// Unenroll stops the agent and removes the machine's identity. The
// server still lists the machine, and keeps its backups, until it is
// removed there.
func (s *Service) Unenroll(ctx context.Context) error {
	s.enrollMu.Lock()
	defer s.enrollMu.Unlock()
	// Remove the identity first, so that the run loop finds the machine
	// unenrolled when the agent stops.
	if err := Unenroll(s.dir); err != nil {
		return err
	}
	s.mu.Lock()
	stop, stopped := s.stop, s.stopped
	s.mu.Unlock()
	if stop != nil {
		stop()
		select {
		case <-stopped:
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
	s.sumMu.Lock()
	s.summary = nil
	s.sumMu.Unlock()
	s.setErr(nil)
	s.nudge()
	return nil
}

// Unenroll removes the identity saved in dir.
func Unenroll(dir string) error {
	// agent.json first: without it the machine is not enrolled.
	for _, name := range []string{"agent.json", "agent.key", "agent.crt", "ca.pem", PreseedFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Service) running() (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil {
		if !Enrolled(s.dir) {
			return nil, errors.New("this machine is not enrolled yet")
		}
		return nil, errors.New("the agent is not running: " + s.err)
	}
	return s.agent, nil
}

// Backup asks the server to back this machine up now.
func (s *Service) Backup(ctx context.Context) (*agentctl.BackupResult, error) {
	a, err := s.running()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var res agentctl.BackupResult
	if err := a.c.once(ctx, "POST", "/agent/v1/backup", &res); err != nil {
		return nil, fmt.Errorf("could not ask the server for a backup: %w", err)
	}
	s.sumMu.Lock()
	s.sumAt = time.Time{} // the policies just changed
	s.sumMu.Unlock()
	if res.Policies == 0 {
		return &res, errors.New("this machine has no backup policy yet; create one in the server's web UI")
	}
	return &res, nil
}

// once sends one request, without the retries of do: someone is waiting.
func (c *client) once(ctx context.Context, method, path string, out any) error {
	req, err := newRequest(ctx, method, c.base+path)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// summaryFor returns the server's view of this machine, a few seconds
// old at most, or nil when the server does not answer.
func (s *Service) summaryFor(ctx context.Context, a *Agent) *api.AgentSummary {
	s.sumMu.Lock()
	defer s.sumMu.Unlock()
	if time.Since(s.sumAt) < 5*time.Second {
		return s.summary
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var sum api.AgentSummary
	if err := a.c.once(ctx, "GET", "/agent/v1/summary", &sum); err != nil {
		s.summary = nil // an older server, or one that is away
	} else {
		s.summary = &sum
	}
	s.sumAt = time.Now()
	return s.summary
}

// Status reports what the agent is doing.
func (s *Service) Status(ctx context.Context) *agentctl.Status {
	st := &agentctl.Status{Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, FullDiskAccess: fullDiskAccess()}
	st.Hostname, _ = os.Hostname()
	st.Binary, _ = os.Executable()
	s.mu.Lock()
	a, serr := s.agent, s.err
	s.mu.Unlock()
	st.Error = serr
	if a == nil {
		if cfg, err := loadConfig(s.dir); err == nil {
			st.Enrolled, st.Server, st.Name, st.AgentID = true, cfg.Server, cfg.Name, cfg.AgentID
		}
		return st
	}
	st.Enrolled, st.Server, st.Name, st.AgentID = true, a.cfg.Server, a.cfg.Name, a.cfg.AgentID
	a.mu.Lock()
	as := a.state
	a.mu.Unlock()
	st.Connected, st.Revoked, st.Error = as.connected, as.revoked, as.err
	if !as.lastContact.IsZero() {
		st.LastContact = &as.lastContact
	}
	if as.job != nil {
		j := *as.job
		if as.progress != nil {
			j.Progress, _ = json.Marshal(as.progress)
		}
		st.Job = &j
		st.Connected = true // it is talking to the server as it works
	}
	if st.Connected {
		if sum := s.summaryFor(ctx, a); sum != nil {
			st.Policies = sum.Policies
			if sum.Name != "" {
				st.Name = sum.Name
			}
		}
	}
	return st
}

// OfflineStatus describes the agent state in dir when no agent is
// running to ask.
func OfflineStatus(dir string) *agentctl.Status {
	st := &agentctl.Status{Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
	st.Hostname, _ = os.Hostname()
	if cfg, err := loadConfig(dir); err == nil {
		st.Enrolled, st.Server, st.Name, st.AgentID = true, cfg.Server, cfg.Name, cfg.AgentID
	}
	st.Error = agentctl.ErrNotRunning.Error()
	return st
}
