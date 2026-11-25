package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/api"
)

const serviceName = "better-backup-agent"

// installKind works out how this agent was put on the machine: the
// installer puts it in Program Files\better-backup\Agent.
func installKind() string {
	exe, err := os.Executable()
	if err != nil {
		return api.InstallManual
	}
	dir := strings.ToLower(filepath.Dir(exe))
	for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
		if pf := os.Getenv(env); pf != "" && dir == strings.ToLower(filepath.Join(pf, "better-backup", "Agent")) {
			return api.InstallMSI
		}
	}
	return api.InstallManual
}

func system32(name string) string {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return name
	}
	return filepath.Join(dir, name)
}

// runInstaller installs the package. Windows Installer undoes a failed
// install by itself, which leaves the earlier version in place.
func runInstaller(ctx context.Context, kind, installer, folder string) error {
	if kind != api.InstallMSI {
		return fmt.Errorf("cannot install a %q package on Windows", kind)
	}
	log := filepath.Join(folder, "msi.log")
	cmd := exec.CommandContext(ctx, system32("msiexec.exe"), "/i", installer, "/qn", "/norestart", "/l*v", log)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3010 { // done; wants a restart of Windows some time
		return nil
	}
	if err != nil {
		return fmt.Errorf("msiexec: %w (its log is %s)", err, log)
	}
	return nil
}

// launchUpdater starts the updater on its own, so that it carries on
// when the installer stops the service.
func launchUpdater(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func serviceRuns() bool {
	out, _ := exec.Command(system32("sc.exe"), "query", serviceName).Output()
	return strings.Contains(string(out), "RUNNING")
}

func stopService() error {
	if !serviceRuns() {
		return nil
	}
	out, err := exec.Command(system32("net.exe"), "stop", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("net stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func restartService() error {
	if serviceRuns() {
		return nil
	}
	out, err := exec.Command(system32("net.exe"), "start", serviceName).CombinedOutput()
	if err != nil && !serviceRuns() {
		return fmt.Errorf("net start: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// verifySigner checks that an installer comes from whoever signed this
// program: an agent with a valid Authenticode signature only installs
// packages validly signed by the same publisher. An agent that is not
// signed has nothing to compare with, and relies on the SHA-256 the
// server gave over the connection it trusts.
func verifySigner(ctx context.Context, exe, installer string) error {
	// The paths go by environment variable: PowerShell joins what follows
	// -Command into one line and splits it again at the spaces.
	script := `$ErrorActionPreference='Stop'; foreach ($f in $env:BB_UPDATE_SELF, $env:BB_UPDATE_NEW) { $s = Get-AuthenticodeSignature -LiteralPath $f; "$($s.Status)|$($s.SignerCertificate.Subject)" }`
	ps := filepath.Join(filepath.Dir(system32("x")), "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// Without PSModulePath, so that Windows PowerShell finds its own
	// modules and not those of a newer PowerShell that started the agent.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "BB_UPDATE_SELF="+exe, "BB_UPDATE_NEW="+installer)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return checkAuthenticode(string(out), err)
}
