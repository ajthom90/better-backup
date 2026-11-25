//go:build !windows

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/ajthom90/better-backup/internal/api"
)

const (
	serviceName = "better-backup-agent"
	macProgram  = "/usr/local/bin/bb-agent"
	macDaemon   = "/Library/LaunchDaemons/better-backup-agent.plist"
)

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// installKind works out how this agent was put on the machine.
func installKind() string {
	if os.Getenv("BB_HOST_ROOT") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != "" || exists("/.dockerenv") || exists("/run/.containerenv") {
		return api.InstallContainer
	}
	exe, err := os.Executable()
	if err != nil || os.Geteuid() != 0 {
		return api.InstallManual
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	switch runtime.GOOS {
	case "darwin":
		if exe == macProgram && exists(macDaemon) {
			return api.InstallPkg
		}
	case "linux":
		// The updater has to leave the service's control group before
		// the package restarts the service; that takes systemd.
		if _, err := exec.LookPath("systemd-run"); err != nil || !exists("/run/systemd/system") {
			return api.InstallManual
		}
		if exec.Command("dpkg-query", "-S", exe).Run() == nil {
			return api.InstallDeb
		}
		if exec.Command("rpm", "-qf", exe).Run() == nil {
			return api.InstallRPM
		}
	}
	return api.InstallManual
}

// tail is the end of what a command printed, for an error message.
func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 600 {
		s = "…" + s[len(s)-600:]
	}
	return s
}

// runInstaller installs the package.
func runInstaller(ctx context.Context, kind, installer, folder string) error {
	var cmd *exec.Cmd
	switch kind {
	case api.InstallDeb:
		cmd = exec.CommandContext(ctx, "dpkg", "-i", installer)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	case api.InstallRPM:
		cmd = exec.CommandContext(ctx, "rpm", "-U", "--replacepkgs", installer)
	case api.InstallPkg:
		cmd = exec.CommandContext(ctx, "/usr/sbin/installer", "-pkg", installer, "-target", "/")
	default:
		return fmt.Errorf("cannot install a %q package on this system", kind)
	}
	out, err := cmd.CombinedOutput()
	os.WriteFile(filepath.Join(folder, "install.log"), out, 0o600)
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(cmd.Path), err, tail(out))
	}
	return nil
}

// launchUpdater starts the updater so that it survives the agent being
// stopped by the installer.
func launchUpdater(exe string, args []string) error {
	if runtime.GOOS == "linux" {
		// systemd stops everything in the service's control group when
		// the service restarts, the package manager included. A unit of
		// its own is outside it.
		unit := serviceName + "-update-" + strconv.Itoa(os.Getpid())
		cmd := exec.Command("systemd-run", append([]string{"--unit=" + unit, "--collect", "--quiet", "--description=better-backup agent update", exe}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-run: %w: %s", err, tail(out))
		}
		return nil
	}
	// launchd stops the processes in the daemon's process group; a
	// session of its own is outside it.
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// stopService is only needed where a running program cannot be replaced.
func stopService() error { return nil }

// restartService starts the installed agent, again if it runs.
func restartService() error {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("launchctl", "kickstart", "-k", "system/"+serviceName).CombinedOutput()
		if err != nil {
			if out2, err2 := exec.Command("launchctl", "bootstrap", "system", macDaemon).CombinedOutput(); err2 != nil {
				return fmt.Errorf("launchctl: %w: %s %s", err2, tail(out), tail(out2))
			}
		}
		return nil
	}
	if out, err := exec.Command("systemctl", "restart", serviceName+".service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart: %w: %s", err, tail(out))
	}
	return nil
}

var teamID = regexp.MustCompile(`(?m)^TeamIdentifier=([A-Z0-9]{10})\s*$`)

// verifySigner checks, on macOS, that an installer comes from whoever
// signed this program: a signed agent only installs packages signed with
// the same Apple developer team. An agent that is not signed (built from
// source, or by CI) has nothing to compare with, and relies on the
// SHA-256 the server gave over the connection it trusts.
func verifySigner(ctx context.Context, exe, installer string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, _ := exec.CommandContext(ctx, "/usr/bin/codesign", "-dv", "--verbose=2", exe).CombinedOutput()
	m := teamID.FindSubmatch(out)
	if m == nil {
		return nil
	}
	team := string(m[1])
	sig, err := exec.CommandContext(ctx, "/usr/sbin/pkgutil", "--check-signature", installer).CombinedOutput()
	return checkPkgSignature(team, sig, err)
}

// checkPkgSignature reads what pkgutil --check-signature said.
func checkPkgSignature(team string, out []byte, err error) error {
	if err != nil {
		return fmt.Errorf("the installer is not signed, but this agent is (Apple team %s); it was not installed: %s", team, tail(out))
	}
	if !bytes.Contains(out, []byte("signed by a developer certificate issued by Apple")) {
		return fmt.Errorf("the installer's signature is not an Apple Developer ID; it was not installed: %s", tail(out))
	}
	if !bytes.Contains(out, []byte("("+team+")")) {
		return errors.New("the installer is signed by someone other than the signer of this agent (Apple team " + team + "); it was not installed")
	}
	return nil
}
