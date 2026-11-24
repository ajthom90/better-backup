// Package agentctl is the agent's local control channel. The running
// agent serves it on a unix socket (a named pipe on Windows), and the
// bb-agent commands and the desktop app use it to see what the agent is
// doing, to enroll the machine and to start a backup.
//
// The agent runs as root and keeps its identity where only root can
// read it. The control channel is what lets an administrator who is not
// root set the agent up: the socket's permissions decide who may.
package agentctl

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/paths"
)

// Status is what the agent reports about itself.
type Status struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`

	Enrolled bool   `json:"enrolled"`
	Server   string `json:"server,omitempty"`
	Name     string `json:"name,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`

	// Connected is true while the server answers the agent.
	Connected   bool       `json:"connected"`
	LastContact *time.Time `json:"last_contact,omitempty"`
	// Error says why the agent is not connected.
	Error string `json:"error,omitempty"`
	// Revoked is true when the server refuses this agent's identity.
	Revoked bool `json:"revoked,omitempty"`

	// Job is the job running now.
	Job *Job `json:"job,omitempty"`
	// Policies are this machine's backup policies, as the server
	// describes them. Absent when the server cannot be reached.
	Policies []Policy `json:"policies,omitempty"`

	// FullDiskAccess is set on macOS: false means the agent has not been
	// given Full Disk Access and cannot read every file.
	FullDiskAccess *bool `json:"full_disk_access,omitempty"`
	// Binary is the path of the agent program, to grant it access.
	Binary string `json:"binary,omitempty"`
}

// Job is a running job.
type Job struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Started  time.Time       `json:"started"`
	Progress json.RawMessage `json:"progress,omitempty"`
}

// Policy summarizes one backup policy of this machine.
type Policy = api.PolicySummary

// EnrollRequest enrolls the machine. Give a Code, or Server, Token and
// Fingerprint.
type EnrollRequest struct {
	Code        string `json:"code,omitempty"`
	Server      string `json:"server,omitempty"`
	Token       string `json:"token,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Name        string `json:"name,omitempty"`
}

// BackupResult says what a request to back up now started.
type BackupResult = api.BackupNow

// Backend is the agent, as the control channel sees it.
type Backend interface {
	Status(ctx context.Context) *Status
	Enroll(ctx context.Context, req EnrollRequest) error
	Unenroll(ctx context.Context) error
	Backup(ctx context.Context) (*BackupResult, error)
}

// SocketPath is where the agent with the given state directory serves
// its control channel: $BB_AGENT_SOCKET, a well-known place for the
// default directory, or inside the directory itself. On Windows it is
// always a file in the directory, which says where the channel is.
func SocketPath(dir string) string {
	if v := os.Getenv("BB_AGENT_SOCKET"); v != "" {
		return v
	}
	if dir == "" {
		dir = paths.AgentDir()
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "control.json")
	}
	if filepath.Clean(dir) == filepath.Clean(paths.AgentDir()) {
		return defaultSocket
	}
	return filepath.Join(dir, "control.sock")
}

type errorBody struct {
	Error string `json:"error"`
}

// Handler serves the control API for a backend.
func Handler(b Backend) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		reply(w, http.StatusBadRequest, errorBody{err.Error()})
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, b.Status(r.Context()))
	})
	mux.HandleFunc("POST /v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req EnrollRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			fail(w, fmt.Errorf("invalid request: %w", err))
			return
		}
		if err := b.Enroll(r.Context(), req); err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, b.Status(r.Context()))
	})
	mux.HandleFunc("POST /v1/unenroll", func(w http.ResponseWriter, r *http.Request) {
		if err := b.Unenroll(r.Context()); err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, b.Status(r.Context()))
	})
	mux.HandleFunc("POST /v1/backup", func(w http.ResponseWriter, r *http.Request) {
		res, err := b.Backup(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, res)
	})
	return mux
}

// guard refuses requests that do not carry the channel's secret, and
// requests a web page could have been tricked into sending. Channels
// protected by file permissions alone have no secret.
func guard(secret string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != clientHost || r.Header.Get("Origin") != "" || (secret != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(errorBody{ErrDenied.Error()})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// clientHost is the host name clients put in their requests. It names
// nothing: a browser that was sent to the channel's port by a hostile
// page would say something else.
const clientHost = "better-backup-agent"

// Serve serves the control API on the channel at path until ctx ends.
func Serve(ctx context.Context, path string, b Backend) error {
	ln, secret, err := listen(path)
	if err != nil {
		return err
	}
	defer cleanup(path)
	srv := &http.Server{Handler: guard(secret, Handler(b)), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// ErrNotRunning means no agent answers on the control channel.
var ErrNotRunning = errors.New("the better-backup agent is not running")

// ErrDenied means this user may not use the control channel.
var ErrDenied = errors.New("this account is not allowed to manage the better-backup agent")

// Client talks to a running agent.
type Client struct{ path string }

// Dial returns a client for the agent serving the channel at path. It
// does not connect until the first call.
func Dial(path string) *Client { return &Client{path: path} }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+clientHost+path, body)
	if err != nil {
		return err
	}
	// Look the channel up for every request: the agent may have been
	// restarted, or only just installed.
	network, addr, secret, err := endpoint(c.path)
	if err == nil {
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
		var resp *http.Response
		if resp, err = tr.RoundTrip(req); err == nil {
			defer resp.Body.Close()
			return decodeReply(resp, out)
		}
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, os.ErrPermission):
		return ErrDenied
	case errors.Is(err, os.ErrNotExist) || isRefused(err):
		return ErrNotRunning
	}
	return err
}

func decodeReply(resp *http.Response, out any) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			if resp.StatusCode == http.StatusForbidden {
				return ErrDenied
			}
			return errors.New(e.Error)
		}
		return fmt.Errorf("the agent answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Status asks the agent what it is doing.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	st := new(Status)
	return st, c.do(ctx, "GET", "/v1/status", nil, st)
}

// Enroll enrolls the machine and returns the new status.
func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (*Status, error) {
	st := new(Status)
	return st, c.do(ctx, "POST", "/v1/enroll", req, st)
}

// Unenroll removes the machine's identity.
func (c *Client) Unenroll(ctx context.Context) error {
	return c.do(ctx, "POST", "/v1/unenroll", nil, nil)
}

// Backup starts a backup of every policy of this machine.
func (c *Client) Backup(ctx context.Context) (*BackupResult, error) {
	res := new(BackupResult)
	return res, c.do(ctx, "POST", "/v1/backup", nil, res)
}
