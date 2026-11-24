// bb-agent is the better-backup agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/cli"
	"github.com/ajthom90/better-backup/internal/paths"
	"github.com/ajthom90/better-backup/internal/recovery"
	"github.com/ajthom90/better-backup/internal/svc"
)

const usage = `bb-agent is the better-backup agent. It runs on each machine to back up.

Usage:
  bb-agent enroll --server URL --token TOKEN --fingerprint FP [--name NAME]
  bb-agent run                      run in the foreground (the default command)
  bb-agent service ACTION           install, uninstall, start, stop, restart, status
  bb-agent version

Get the enroll command, with its token and fingerprint, from the server:
  bb token create

Flags:
  --dir DIR   agent state directory (default %s, or $BB_AGENT_DIR)

Run the agent as root (Linux, macOS) or as a service (Windows) so it can read
every file. On macOS also grant it Full Disk Access.
`

func main() {
	// On the recovery disc this program is /init, the machine's first
	// process.
	if filepath.Base(os.Args[0]) == "init" || (len(os.Args) > 1 && os.Args[1] == "recover") {
		recovery.Main(agent.Version)
		return
	}
	fs := flag.NewFlagSet("bb-agent", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, paths.AgentDir()) }
	dir := fs.String("dir", paths.AgentDir(), "")
	serverURL := fs.String("server", os.Getenv("BB_SERVER"), "")
	token := fs.String("token", os.Getenv("BB_ENROLL_TOKEN"), "")
	fp := fs.String("fingerprint", os.Getenv("BB_FINGERPRINT"), "")
	host, _ := os.Hostname()
	name := fs.String("name", host, "")
	pos := cli.Parse(fs, os.Args[1:])
	cmd := "run"
	if len(pos) > 0 {
		cmd = pos[0]
	}
	spec := svc.Spec{Name: "better-backup-agent", DisplayName: "better-backup agent",
		Description: "better-backup agent: backs up and restores this machine.", Args: []string{"run", "--dir", *dir}}

	enroll := func() error {
		if *serverURL == "" || *token == "" || *fp == "" {
			return errors.New("enroll needs --server, --token and --fingerprint; create them with: bb token create")
		}
		cfg, err := agent.Enroll(*dir, *serverURL, *token, *fp, *name)
		if err != nil {
			return err
		}
		fmt.Printf("enrolled as %q (agent %s) with %s\n", cfg.Name, cfg.AgentID, cfg.Server)
		return nil
	}

	var err error
	switch cmd {
	case "enroll":
		if agent.Enrolled(*dir) {
			err = fmt.Errorf("already enrolled; delete %s to enroll again", *dir)
		} else {
			err = enroll()
		}
	case "run":
		// Containers enroll on first start from environment variables.
		if !agent.Enrolled(*dir) && *serverURL != "" && *token != "" && *fp != "" {
			if err = enroll(); err != nil {
				break
			}
		}
		err = svc.Run(spec, func(ctx context.Context) error {
			a, err := agent.New(*dir, slog.Default())
			if err != nil {
				return err
			}
			return a.Run(ctx)
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
