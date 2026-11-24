//go:build !windows

package agentctl

import (
	"errors"
	"net"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const defaultSocket = "/var/run/better-backup-agent.sock"

// Group is the Linux group whose members may manage the agent without
// being root. It is optional: without it, only root may.
const Group = "better-backup"

// listen opens the control socket. Who may connect is decided by the
// socket's owner and mode: root always; on macOS also administrators
// (the admin group), on Linux also members of the better-backup group
// when that group exists.
func listen(path string) (net.Listener, string, error) {
	// A socket left by an agent that was killed is in the way; one that
	// answers belongs to an agent that is still running.
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, "", errors.New("another agent is already serving " + path)
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path)
	}
	// Create it private, then open it up: at no time can anyone else
	// connect.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, "", err
	}
	if os.Geteuid() == 0 {
		gid := -1
		switch runtime.GOOS {
		case "darwin":
			gid = 80 // admin
		default:
			if g, err := user.LookupGroup(Group); err == nil {
				gid, _ = strconv.Atoi(g.Gid)
			}
		}
		if gid > 0 && os.Chown(path, 0, gid) == nil {
			os.Chmod(path, 0o660)
		}
	}
	return ln, "", nil
}

func cleanup(path string) { os.Remove(path) }

func endpoint(path string) (network, addr, secret string, err error) {
	if _, err := os.Stat(path); err != nil {
		return "", "", "", err
	}
	return "unix", path, "", nil
}

func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK)
}
