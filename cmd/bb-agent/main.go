// bb-agent is the better-backup agent.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/agentctl"
	"github.com/ajthom90/better-backup/internal/agentui"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/cli"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/paths"
	"github.com/ajthom90/better-backup/internal/svc"
	"github.com/ajthom90/better-backup/internal/tree"
)

const usage = `bb-agent is the better-backup agent. It runs on each machine to back up.

Usage:
  bb-agent enroll [CODE] [--name NAME]   enroll this machine with a server
  bb-agent status [--json]               what the agent is doing
  bb-agent backup                        back this machine up now
  bb-agent unenroll                      forget the server
  bb-agent setup                         do all of that on a web page instead
  bb-agent run                           run in the foreground (the default command)
  bb-agent service ACTION                install, uninstall, start, stop, restart, status
  bb-agent version

Get the enrollment code from the server: choose "Add an agent" in its web
UI, or run "bb token create". Without a code, enroll asks for it. The
three parts of a code can also be given one by one:
  bb-agent enroll --server URL --token TOKEN --fingerprint FP

Flags:
  --dir DIR   agent state directory (default %s, or $BB_AGENT_DIR)

The installed agent runs as root (Linux, macOS) or as a service (Windows)
so that it can read every file; these commands talk to it, and need root
or an administrator too. On macOS also grant bb-agent Full Disk Access.
`

var (
	dir     string
	ctl     *agentctl.Client
	timeout = 2 * time.Minute // enrolling waits for the server
)

func main() {
	fs := flag.NewFlagSet("bb-agent", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, paths.AgentDir()) }
	fs.StringVar(&dir, "dir", paths.AgentDir(), "")
	var req agentctl.EnrollRequest
	fs.StringVar(&req.Code, "code", os.Getenv("BB_ENROLL_CODE"), "")
	fs.StringVar(&req.Server, "server", os.Getenv("BB_SERVER"), "")
	fs.StringVar(&req.Token, "token", os.Getenv("BB_ENROLL_TOKEN"), "")
	fs.StringVar(&req.Fingerprint, "fingerprint", os.Getenv("BB_FINGERPRINT"), "")
	fs.StringVar(&req.Name, "name", os.Getenv("BB_AGENT_NAME"), "")
	asJSON := fs.Bool("json", false, "")
	noBrowser := fs.Bool("no-browser", false, "")
	pos := cli.Parse(fs, os.Args[1:])
	cmd := "run"
	if len(pos) > 0 {
		cmd = pos[0]
	}
	spec := svc.Spec{Name: "better-backup-agent", DisplayName: "better-backup agent",
		Description: "better-backup agent: backs up and restores this machine.", Args: []string{"run", "--dir", dir},
		LogFile: filepath.Join(dir, "agent.log")}
	ctl = agentctl.Dial(agentctl.SocketPath(dir))
	given := req.Code != "" || (req.Server != "" && req.Token != "" && req.Fingerprint != "")

	var err error
	switch cmd {
	case "enroll":
		if len(pos) > 1 {
			req.Code = pos[1]
			given = true
		}
		err = enroll(req, given)
	case "status":
		err = status(*asJSON)
	case "backup":
		err = backup()
	case "unenroll":
		err = unenroll()
	case "setup":
		err = setup(*noBrowser)
	case "fda-check":
		// Run by the agent itself on macOS: see watchFullDiskAccess.
		if ok := agent.FullDiskAccess(); ok == nil || !*ok {
			os.Exit(1)
		}
	case "run":
		// Containers enroll on first start from environment variables.
		if !agent.Enrolled(dir) && given {
			var cfg *agent.Config
			if cfg, err = agent.EnrollWith(dir, req); err != nil {
				break
			}
			fmt.Printf("enrolled as %q (agent %s) with %s\n", cfg.Name, cfg.AgentID, cfg.Server)
		}
		err = svc.Run(spec, func(ctx context.Context) error {
			return agent.NewService(dir, slog.Default()).Run(ctx)
		})
	case "service":
		if len(pos) < 2 {
			err = errors.New("usage: bb-agent service install|uninstall|start|stop|restart|status")
		} else {
			err = svc.Control(spec, pos[1])
		}
	case "version":
		fmt.Println("bb-agent", agent.Version, runtime.GOOS+"/"+runtime.GOARCH)
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		cli.Fatal(err)
	}
}

// denied explains a refusal by the operating system.
func denied(err error) error {
	if !errors.Is(err, agentctl.ErrDenied) && !errors.Is(err, os.ErrPermission) {
		return err
	}
	if runtime.GOOS == "windows" {
		return errors.New("this needs an administrator: run it from an elevated prompt")
	}
	return fmt.Errorf("this needs root: run it with sudo (%w)", err)
}

func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func enroll(req agentctl.EnrollRequest, given bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Say so before asking for a code that cannot be used.
	st, serr := ctl.Status(ctx)
	if errors.Is(serr, agentctl.ErrDenied) {
		return denied(serr)
	}
	if (serr == nil && st.Enrolled) || (serr != nil && agent.Enrolled(dir)) {
		return errors.New("this machine is already enrolled; to enroll it again, first run: bb-agent unenroll")
	}
	if !given {
		if !interactive() {
			return errors.New("enroll needs the enrollment code: bb-agent enroll CODE\nGet it from \"Add an agent\" in the server's web UI, or from: bb token create")
		}
		in := bufio.NewReader(os.Stdin)
		host, _ := os.Hostname()
		fmt.Println("Enroll this machine with a better-backup server.")
		fmt.Println("In the server's web UI choose \"Add an agent\" and copy the enrollment code.")
		fmt.Println()
		for req.Code == "" {
			fmt.Print("Enrollment code: ")
			line, err := in.ReadString('\n')
			req.Code = strings.TrimSpace(line)
			if err != nil && req.Code == "" {
				return errors.New("no enrollment code given")
			}
			if req.Code != "" {
				if _, _, _, err := api.DecodeEnrollCode(req.Code); err != nil {
					fmt.Println(" ", err)
					req.Code = ""
				}
			}
		}
		if req.Name == "" {
			fmt.Printf("Name for this machine [%s]: ", host)
			line, _ := in.ReadString('\n')
			req.Name = strings.TrimSpace(line)
		}
	}
	if serr == nil {
		// The running agent enrolls, and starts work at once.
		st, err := ctl.Enroll(ctx, req)
		if err != nil {
			return denied(err)
		}
		fmt.Printf("Enrolled as %q with %s.\n", st.Name, st.Server)
		for i := 0; i < 40 && !st.Connected && st.Error == ""; i++ {
			time.Sleep(250 * time.Millisecond)
			if st, err = ctl.Status(ctx); err != nil {
				return err
			}
		}
		if st.Connected {
			fmt.Println("The agent is connected. Create a backup policy for this machine in the server's web UI.")
		} else {
			fmt.Println("The agent is not connected yet:", st.Error)
		}
		fullDiskAccessHint(st)
		return nil
	}
	cfg, err := agent.EnrollWith(dir, req)
	if err != nil {
		return denied(err)
	}
	fmt.Printf("Enrolled as %q with %s.\n", cfg.Name, cfg.Server)
	fmt.Println("The agent is not running. Start it with: bb-agent service start")
	fmt.Println("(or, if it is not installed as a service yet: bb-agent service install && bb-agent service start)")
	return nil
}

func fullDiskAccessHint(st *agentctl.Status) {
	if st.FullDiskAccess == nil || *st.FullDiskAccess {
		return
	}
	fmt.Println()
	fmt.Println("macOS is keeping some files from the agent (Mail, Messages, Safari, Photos and")
	fmt.Println("other apps' data). To back them up, open System Settings, Privacy & Security,")
	fmt.Println("Full Disk Access, press +, and add:", st.Binary)
	fmt.Println("(press Cmd+Shift+G in the file dialog to type that path), then run:")
	fmt.Println("  sudo bb-agent service restart")
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func status(asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := ctl.Status(ctx)
	running := err == nil
	if errors.Is(err, agentctl.ErrDenied) {
		return denied(err)
	}
	if err != nil {
		st = agent.OfflineStatus(dir)
	}
	if asJSON {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		if !running {
			os.Exit(1)
		}
		return nil
	}
	fmt.Printf("better-backup agent %s (%s/%s)\n", st.Version, st.OS, st.Arch)
	row := func(k, v string) { fmt.Printf("  %-10s %s\n", k+":", v) }
	switch {
	case !running && st.Enrolled:
		row("Agent", "not running. Start it with: bb-agent service start")
	case !running:
		row("Agent", "not running, and this machine is not enrolled. Enroll it with: bb-agent enroll")
	case !st.Enrolled && st.Error != "":
		row("Agent", "running, not enrolled: "+st.Error)
	case !st.Enrolled:
		row("Agent", "running, waiting to be enrolled. Enroll this machine with: bb-agent enroll")
	}
	if st.Enrolled {
		row("Machine", st.Name)
		row("Server", st.Server)
	}
	if running && st.Enrolled {
		switch {
		case st.Revoked:
			row("State", "the server no longer accepts this machine. If it was removed there, run: bb-agent unenroll")
		case st.Connected && st.LastContact != nil:
			row("State", "connected (last contact "+ago(*st.LastContact)+")")
		case st.Connected:
			row("State", "connected")
		case st.Error != "":
			row("State", "not connected: "+st.Error)
		default:
			row("State", "connecting")
		}
		if j := st.Job; j != nil {
			line := j.Type + " running, started " + ago(j.Started)
			var p tree.Stats
			if j.Type == api.JobBackup && json.Unmarshal(j.Progress, &p) == nil && p.Files > 0 {
				line += fmt.Sprintf(": %d files, %s read, %s sent", p.Files, cli.Size(p.BytesRead), cli.Size(p.BytesUploaded))
			}
			row("Now", line)
		}
		if st.Connected && len(st.Policies) == 0 {
			row("Backups", "no backup policy yet; create one for this machine in the server's web UI")
		}
		for i, p := range st.Policies {
			line := p.Name
			if p.Schedule != "" {
				line += ", " + p.Schedule
			}
			if p.Paused {
				line += ", paused"
			}
			switch {
			case p.LastFinished == nil:
				line += ": no backup yet"
			case p.LastStatus == api.StatusFailed:
				line += ": last backup failed " + ago(*p.LastFinished) + " (" + p.LastError + ")"
				if p.LastSuccess != nil {
					line += "; last good backup " + ago(*p.LastSuccess)
				}
			case p.LastStatus == api.StatusWarning:
				line += ": last backup " + ago(*p.LastFinished) + ", with warnings"
			default:
				line += ": last backup " + ago(*p.LastFinished)
			}
			if p.Pending {
				line += "; one is queued or running"
			}
			row(map[bool]string{true: "Backups", false: ""}[i == 0], line)
		}
	}
	if running {
		fullDiskAccessHint(st)
	}
	if !running {
		os.Exit(1)
	}
	return nil
}

func backup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := ctl.Backup(ctx)
	if err != nil {
		if errors.Is(err, agentctl.ErrNotRunning) {
			return errors.New("the agent is not running; start it with: bb-agent service start")
		}
		return denied(err)
	}
	if res.Started == 0 {
		fmt.Println("A backup of this machine is already queued or running. Follow it with: bb-agent status")
		return nil
	}
	fmt.Printf("Started %d of this machine's %d backup policies. Follow it with: bb-agent status\n", res.Started, res.Policies)
	return nil
}

func unenroll() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := ctl.Unenroll(ctx)
	if errors.Is(err, agentctl.ErrNotRunning) {
		err = agent.Unenroll(dir)
	}
	if err != nil {
		return denied(err)
	}
	fmt.Println("This machine is no longer enrolled. Its backups stay on the server; remove the")
	fmt.Println("machine there, in the web UI, if it is not coming back. Enroll again with: bb-agent enroll")
	return nil
}

// setup serves the settings page on a local port and opens it in the
// browser: the same page the desktop app shows, for machines that have
// no app. The address carries a key, so that nobody else on the machine
// can use the page.
func setup(noBrowser bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := ctl.Status(ctx); errors.Is(err, agentctl.ErrDenied) {
		return denied(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	key := keys.RandomHex(16)
	addr := "http://" + ln.Addr().String() + "/?key=" + key
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second,
		Handler: agentui.Handler(agentui.Options{Control: ctl, Key: key, Host: ln.Addr().String()})}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	fmt.Println("The settings page of the better-backup agent is at:")
	fmt.Println()
	fmt.Println("  " + addr)
	fmt.Println()
	fmt.Println("It works from this machine only, until you stop this command with Ctrl+C.")
	// A browser started by root, from sudo, is the wrong user's browser.
	if !noBrowser && os.Getenv("SUDO_USER") == "" {
		if agentui.OpenBrowser(addr) == nil {
			fmt.Println("Opening it in your browser.")
		}
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
