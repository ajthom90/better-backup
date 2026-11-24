// Package agentui is the agent's settings page: one web page, and the
// handler behind it, that enrolls the machine, shows what the agent is
// doing and starts a backup. The desktop app shows it in a window;
// "bb-agent setup" serves it to a browser where there is no app.
//
// The page never touches the agent's files: everything goes through the
// agent's local control channel (package agentctl).
package agentui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/agentctl"
)

//go:embed ui/index.html
var uiFS embed.FS

// Options says how the page is being shown.
type Options struct {
	// Control reaches the agent.
	Control *agentctl.Client
	// App is true inside the desktop app, false in a browser.
	App bool
	// Key, when set, must come with every request: first in the address
	// the page is opened by (?key=...), then as a cookie. It keeps other
	// users of the machine, and other web pages, away from a page served
	// on a local port.
	Key string
	// Host, when set, is the only host name requests may name.
	Host string
	// Open shows an address or a file to the user. The default asks the
	// operating system.
	Open func(target string, reveal bool) error
}

// State is what the page is told.
type State struct {
	// Running is false when no agent answers on this machine.
	Running bool `json:"running"`
	// Denied is true when this user may not manage the agent.
	Denied bool   `json:"denied"`
	Error  string `json:"error,omitempty"`
	// Platform is the operating system: darwin, windows or linux.
	Platform string `json:"platform"`
	App      bool   `json:"app"`
	// Group is the group whose members may manage the agent on Linux.
	Group  string           `json:"group,omitempty"`
	Status *agentctl.Status `json:"status,omitempty"`
}

const cookie = "bb-agent-key"

// Handler serves the page and its API.
func Handler(o Options) http.Handler {
	if o.Open == nil {
		o.Open = open
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	state := func(ctx context.Context) *State {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		st := &State{Platform: runtime.GOOS, App: o.App}
		if runtime.GOOS == "linux" {
			st.Group = "better-backup"
		}
		status, err := o.Control.Status(ctx)
		switch {
		case err == nil:
			st.Running, st.Status = true, status
		case errors.Is(err, agentctl.ErrDenied):
			st.Denied = true
		case errors.Is(err, agentctl.ErrNotRunning):
		default:
			st.Error = err.Error()
		}
		return st
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, state(r.Context()))
	})
	mux.HandleFunc("POST /api/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			fail(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if _, err := o.Control.Enroll(ctx, agentctl.EnrollRequest{Code: req.Code, Name: req.Name}); err != nil {
			fail(w, err)
			return
		}
		// Give the agent a moment to reach the server, so that the page
		// can say "connected" rather than flash "connecting".
		st := state(r.Context())
		for i := 0; i < 20 && st.Status != nil && !st.Status.Connected && st.Status.Error == ""; i++ {
			time.Sleep(250 * time.Millisecond)
			st = state(r.Context())
		}
		reply(w, http.StatusOK, st)
	})
	mux.HandleFunc("POST /api/backup", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err := o.Control.Backup(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/unenroll", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := o.Control.Unenroll(ctx); err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, state(r.Context()))
	})
	// The page may ask for three things to be shown, and no others: it
	// names them, it does not pass addresses.
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			What string `json:"what"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			fail(w, err)
			return
		}
		st := state(r.Context())
		var err error
		switch {
		case req.What == "full-disk-access" && runtime.GOOS == "darwin":
			err = o.Open("x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles", false)
		case req.What == "agent-program" && st.Status != nil && st.Status.Binary != "":
			err = o.Open(st.Status.Binary, true)
		case req.What == "server" && st.Status != nil && strings.HasPrefix(st.Status.Server, "https://"):
			if _, perr := url.Parse(st.Status.Server); perr != nil {
				err = perr
				break
			}
			err = o.Open(st.Status.Server, false)
		default:
			err = errors.New("nothing to open")
		}
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, struct{}{})
	})
	return guard(o, mux)
}

// guard keeps strangers out when the page is served on a local port.
func guard(o Options, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.Host != "" && r.Host != o.Host {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		// A page on another site cannot set this header on a request it
		// makes a browser send here.
		if r.Method != http.MethodGet && r.Header.Get("X-Better-Backup") != "1" {
			http.Error(w, "not from the settings page", http.StatusForbidden)
			return
		}
		if o.Key != "" {
			if k := r.URL.Query().Get("key"); k != "" && r.Method == http.MethodGet && r.URL.Path == "/" {
				if subtle.ConstantTimeCompare([]byte(k), []byte(o.Key)) == 1 {
					http.SetCookie(w, &http.Cookie{Name: cookie, Value: o.Key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
					http.Redirect(w, r, "/", http.StatusSeeOther)
					return
				}
			}
			c, err := r.Cookie(cookie)
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(o.Key)) != 1 {
				http.Error(w, "Open this page with the address that \"bb-agent setup\" printed.", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// open shows an address, or reveals a file, with the operating system's
// own tool.
func open(target string, reveal bool) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if reveal {
			cmd = exec.Command("open", "-R", target)
		} else {
			cmd = exec.Command("open", target)
		}
	case "windows":
		if reveal {
			cmd = exec.Command("explorer.exe", "/select,"+target)
		} else {
			cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target)
		}
	default:
		if reveal {
			return errors.New("not available here")
		}
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// OpenBrowser opens an address in the user's browser.
func OpenBrowser(addr string) error { return open(addr, false) }
