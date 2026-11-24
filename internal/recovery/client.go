// Package recovery is the program on the recovery disc. A machine
// started from the disc has no identity and nothing on its disks worth
// trusting: it finds the server, shows a code on its screen, and once the
// administrator has approved the machine showing that code it becomes an
// agent for as long as it takes to receive a whole-machine restore.
package recovery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
)

// State is what the machine's screen shows.
type State struct {
	Server      string
	Fingerprint string
	Code        string
	Approved    bool
	// Status says in a sentence what the machine is doing or waiting for.
	Status string
	// Problem is the last thing that went wrong, if it still applies.
	Problem string
}

// Options configures Run.
type Options struct {
	Config  api.RecoveryConfig
	Version string
	// Dir holds the identity the machine gets once approved. It is
	// emptied whenever the machine has to start over.
	Dir string
	// Hardware describes the machine. It is called again after a rescan
	// and after every restore.
	Hardware func() *api.RecoveryHardware
	// Command carries out "reboot" or "poweroff".
	Command func(cmd string)
	// Show is called whenever the state changes.
	Show func(State)
	// Clock, if set, is given the server's time before any certificate
	// is checked: the clock of a machine with blank disks is often wrong.
	Clock func(time.Time)
	Log   *slog.Logger
}

type runner struct {
	o     Options
	mu    sync.Mutex
	state State
	// resend makes the next poll carry the hardware again.
	resend bool
}

func (r *runner) set(fn func(*State)) {
	r.mu.Lock()
	fn(&r.state)
	st := r.state
	r.mu.Unlock()
	if r.o.Show != nil {
		r.o.Show(st)
	}
}

func size(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// fetchCA gets the server's certificate authority. With a fingerprint it
// must match. Without one (the address was typed in by hand) whatever
// the server presents is taken and its fingerprint is put on the screen
// for the administrator to compare with the one the web interface shows.
func fetchCA(c api.RecoveryConfig, clock func(time.Time)) (caPEM []byte, fingerprint string, err error) {
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	defer hc.CloseIdleConnections()
	resp, err := hc.Get(c.Server + "/ca.pem")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("%s answered %s; is it a better-backup server?", c.Server, resp.Status)
	}
	if caPEM, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
		return nil, "", err
	}
	if fingerprint, err = api.Fingerprint(caPEM); err != nil {
		return nil, "", fmt.Errorf("%s did not answer like a better-backup server", c.Server)
	}
	if c.Fingerprint != "" && !strings.EqualFold(fingerprint, strings.TrimSpace(c.Fingerprint)) {
		return nil, "", fmt.Errorf("the server at %s is not the one this recovery disc was made for (its fingerprint is %s)", c.Server, fingerprint)
	}
	if clock != nil {
		if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
			clock(t)
		}
	}
	return caPEM, fingerprint, nil
}

var errGone = errors.New("the server no longer knows this machine")

// Run announces the machine to the server and serves restores until ctx
// ends. It never gives up: whatever goes wrong is shown and tried again.
func Run(ctx context.Context, o Options) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	o.Config.Server = strings.TrimRight(o.Config.Server, "/")
	r := &runner{o: o}
	r.set(func(s *State) {
		s.Server, s.Fingerprint, s.Status = o.Config.Server, o.Config.Fingerprint, "Connecting to the server"
	})
	for backoff := time.Second; ctx.Err() == nil; {
		err := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil || errors.Is(err, errGone) {
			backoff = time.Second
			r.set(func(s *State) {
				s.Code, s.Approved, s.Problem, s.Status = "", false, "", "The server forgot this machine (it may have been restarted). Starting over"
			})
		} else {
			r.set(func(s *State) { s.Problem, s.Status = err.Error(), "Cannot reach the server; trying again" })
			backoff = min(backoff*2, 15*time.Second)
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
	}
}

// session runs one session with the server, from announcing the machine
// to the server forgetting it.
func (r *runner) session(ctx context.Context) error {
	o := r.o
	caPEM, fp, err := fetchCA(o.Config, o.Clock)
	if err != nil {
		return err
	}
	tlsCfg, err := api.PinnedTLS(caPEM, nil)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	defer hc.CloseIdleConnections()
	call := func(path, secret string, in, out any) error {
		body, _ := json.Marshal(in)
		req, err := http.NewRequestWithContext(ctx, "POST", o.Config.Server+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return errGone
		}
		if resp.StatusCode >= 300 {
			var e api.Error
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			if json.Unmarshal(b, &e) == nil && e.Error != "" {
				return errors.New("server: " + e.Error)
			}
			return fmt.Errorf("server returned %s", resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	var sess api.RecoverySession
	if err := call("/agent/v1/recovery/sessions", "", struct{}{}, &sess); err != nil {
		return err
	}
	r.set(func(s *State) {
		s.Fingerprint, s.Code, s.Approved, s.Problem = fp, sess.Code, false, ""
		s.Status = "Waiting to be approved in the better-backup web interface"
	})

	// Whatever identity an earlier session left behind is of no use now.
	os.RemoveAll(o.Dir)
	actx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	close(agentDone)
	defer func() {
		stopAgent()
		<-agentDone
		os.RemoveAll(o.Dir)
	}()
	r.resend = true
	failures := 0
	for ctx.Err() == nil {
		poll := api.RecoveryPoll{Version: o.Version}
		r.mu.Lock()
		poll.Status = r.state.Status
		send := r.resend
		r.resend = false
		r.mu.Unlock()
		if send && o.Hardware != nil {
			poll.Hardware = o.Hardware()
		}
		var reply api.RecoveryReply
		if err := call("/agent/v1/recovery/sessions/"+sess.ID+"/poll?wait=5", sess.Secret, &poll, &reply); err != nil {
			if errors.Is(err, errGone) || ctx.Err() != nil {
				return err
			}
			// A restore may be running; a network hiccup must not end it.
			failures++
			r.mu.Lock()
			r.resend = r.resend || send
			r.mu.Unlock()
			r.set(func(s *State) { s.Problem = "Lost contact with the server: " + err.Error() })
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(min(failures, 10)) * time.Second):
			}
			continue
		}
		if failures > 0 {
			failures = 0
			r.set(func(s *State) { s.Problem = "" })
		}
		if reply.EnrollToken != "" {
			if _, err := agent.Enroll(o.Dir, o.Config.Server, reply.EnrollToken, fp, "recovery"); err != nil {
				return fmt.Errorf("the machine was approved but could not sign on: %w", err)
			}
			a, err := agent.New(o.Dir, o.Log)
			if err != nil {
				return err
			}
			a.Notify = r.jobEvent
			agentDone = make(chan struct{})
			go func(done chan struct{}) { defer close(done); a.Run(actx) }(agentDone)
			r.set(func(s *State) {
				s.Approved, s.Status = true, "Approved. Choose the backup to restore in the web interface"
			})
		}
		switch reply.Command {
		case "rescan":
			r.mu.Lock()
			r.resend = true
			r.mu.Unlock()
		case "reboot", "poweroff":
			r.set(func(s *State) {
				s.Status = map[string]string{"reboot": "Restarting", "poweroff": "Switching off"}[reply.Command]
			})
			if o.Command != nil {
				stopAgent()
				<-agentDone
				o.Command(reply.Command)
			}
		}
	}
	return ctx.Err()
}

// jobEvent turns what the agent does into what the screen says.
func (r *runner) jobEvent(event string, j *api.Job, detail any) {
	if j.Type != api.JobRestore {
		return
	}
	switch event {
	case "started":
		r.set(func(s *State) { s.Problem, s.Status = "", "Restoring. Do not switch the machine off" })
	case "progress":
		if b, err := json.Marshal(detail); err == nil {
			var st struct {
				Bytes int64 `json:"bytes"`
			}
			if json.Unmarshal(b, &st) == nil {
				r.set(func(s *State) { s.Status = "Restoring: " + size(st.Bytes) + " written. Do not switch the machine off" })
			}
		}
	case "finished":
		r.mu.Lock()
		r.resend = true
		r.mu.Unlock()
		if err, _ := detail.(error); err != nil {
			r.set(func(s *State) {
				s.Problem, s.Status = err.Error(), "The restore failed. It can be started again from the web interface"
			})
		} else {
			r.set(func(s *State) {
				s.Status = "The restore is finished. Remove the recovery disc and restart the machine"
			})
		}
	}
}
