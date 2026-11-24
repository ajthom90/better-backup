package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// fullDiskAccess reports whether macOS lets this process read files it
// protects: Mail, Messages, Safari data, other apps' data, and so on.
// The privacy database itself is one such file, so trying to open it
// tells. Root does not help: this needs the user's consent in System
// Settings, under Privacy & Security, Full Disk Access.
func fullDiskAccess() *bool {
	f, err := os.Open("/Library/Application Support/com.apple.TCC/TCC.db")
	var ok bool
	switch {
	case err == nil:
		f.Close()
		ok = true
	case errors.Is(err, os.ErrPermission):
		ok = false
	default:
		return nil // cannot tell
	}
	return &ok
}

// FullDiskAccess is fullDiskAccess for other programs: "bb-agent
// fda-check" runs it in a new process.
func FullDiskAccess() *bool { return fullDiskAccess() }

// watchFullDiskAccess restarts the agent once the user has allowed Full
// Disk Access. macOS decides what a process may read when it starts, so
// the running agent does not gain the access it was just given, and
// cannot see that it was given. A new process can: this starts one
// every half minute to look, and when it reports access, ends the agent
// so that launchd starts it again. It only does so under launchd, and
// never in the middle of a job.
func (s *Service) watchFullDiskAccess(ctx context.Context) {
	if os.Getppid() != 1 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if ok := fullDiskAccess(); ok == nil || *ok {
			return // nothing to wait for
		}
		if exec.CommandContext(ctx, exe, "fda-check").Run() != nil {
			continue // not allowed yet
		}
		s.mu.Lock()
		a := s.agent
		s.mu.Unlock()
		if a != nil {
			a.mu.Lock()
			working := a.state.job != nil
			a.mu.Unlock()
			if working {
				continue
			}
		}
		s.log.Info("Full Disk Access was allowed; restarting to use it")
		os.Exit(0)
	}
}
