// bb-server is the better-backup server.
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
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/ajthom90/better-backup/internal/cli"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/paths"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/svc"
	"github.com/ajthom90/better-backup/internal/tree"
)

const usage = `bb-server is the better-backup server.

Usage:
  bb-server serve [flags]            run the server (the default command)
  bb-server service ACTION [flags]   install, uninstall, start, stop, restart, status
  bb-server snapshots [flags]        list snapshots straight from the repository
  bb-server restore [flags]          restore straight from the repository, without
                                     a running server or agent (disaster recovery)
  bb-server version

Flags for serve and service install:
  --data DIR       server state (default %s, or $BB_DATA)
  --repo DIR       repository location (default DATA/repo, or $BB_REPO)
  --listen ADDR    listen address (default :8443, or $BB_LISTEN)
  --tls-name NAME  extra DNS name or IP for the TLS certificate; repeatable
                   (or $BB_TLS_NAMES, comma separated)

Flags for restore:
  --snapshot ID    snapshot to restore (required)
  --target DIR     directory to restore into (required)
  --path PATH      restore only this original path; repeatable

The repository passphrase is read from $BB_REPO_PASSPHRASE, or from the key
file DATA/repo.key that the server creates on first start.
`

type options struct {
	data, repo, listen string
	names              cli.Multi
	snapshot, target   string
	include            cli.Multi
}

func parse(args []string) (*options, []string) {
	o := new(options)
	fs := flag.NewFlagSet("bb-server", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, paths.ServerData()) }
	fs.StringVar(&o.data, "data", paths.ServerData(), "")
	fs.StringVar(&o.repo, "repo", os.Getenv("BB_REPO"), "")
	fs.StringVar(&o.listen, "listen", cli.Env("BB_LISTEN", ":8443"), "")
	fs.Var(&o.names, "tls-name", "")
	fs.StringVar(&o.snapshot, "snapshot", "", "")
	fs.StringVar(&o.target, "target", "", "")
	fs.Var(&o.include, "path", "")
	pos := cli.Parse(fs, args)
	if v := os.Getenv("BB_TLS_NAMES"); v != "" {
		o.names = append(o.names, strings.Split(v, ",")...)
	}
	if o.repo == "" {
		o.repo = filepath.Join(o.data, "repo")
	}
	return o, pos
}

func (o *options) spec() svc.Spec {
	args := []string{"serve", "--data", o.data, "--repo", o.repo, "--listen", o.listen}
	for _, n := range o.names {
		args = append(args, "--tls-name", n)
	}
	return svc.Spec{Name: "better-backup-server", DisplayName: "better-backup server",
		Description: "better-backup server: stores backups and coordinates agents.", Args: args}
}

func main() {
	o, pos := parse(os.Args[1:])
	cmd := "serve"
	if len(pos) > 0 {
		cmd = pos[0]
	}
	var err error
	switch cmd {
	case "serve":
		err = svc.Run(o.spec(), func(ctx context.Context) error {
			s, err := server.New(server.Config{DataDir: o.data, RepoDir: o.repo, Listen: o.listen, TLSNames: o.names,
				Passphrase: os.Getenv("BB_REPO_PASSPHRASE"), Log: slog.Default()})
			if err != nil {
				return err
			}
			defer s.Close()
			return s.Serve(ctx)
		})
	case "service":
		if len(pos) < 2 {
			err = errors.New("usage: bb-server service install|uninstall|start|stop|restart|status")
		} else {
			err = svc.Control(o.spec(), pos[1])
		}
	case "snapshots":
		err = offline(o, false)
	case "restore":
		err = offline(o, true)
	case "version":
		fmt.Println("bb-server", server.Version, runtime.GOOS+"/"+runtime.GOARCH)
	default:
		fmt.Fprintf(os.Stderr, usage, paths.ServerData())
		os.Exit(2)
	}
	if err != nil {
		cli.Fatal(err)
	}
}

func passphrase(o *options) ([]byte, error) {
	if p := os.Getenv("BB_REPO_PASSPHRASE"); p != "" {
		return []byte(p), nil
	}
	b, err := os.ReadFile(filepath.Join(o.data, "repo.key"))
	if err != nil {
		return nil, fmt.Errorf("no repository passphrase: set BB_REPO_PASSPHRASE or put the key file at %s", filepath.Join(o.data, "repo.key"))
	}
	return []byte(strings.TrimSpace(string(b))), nil
}

// offline reads the repository directly. It needs only the repository
// directory and its passphrase, which is the point: it works when the
// server's database, certificates and agents are all gone.
func offline(o *options, doRestore bool) error {
	pass, err := passphrase(o)
	if err != nil {
		return err
	}
	r, err := repo.OpenReadOnly(o.repo, pass)
	if err != nil {
		return err
	}
	defer r.Close()
	snaps, err := r.LoadSnapshots()
	if err != nil {
		return err
	}
	if !doRestore {
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SNAPSHOT\tTIME\tHOST\tFILES\tSIZE\tPATHS")
		for _, s := range snaps {
			var names []string
			for _, src := range s.Sources {
				names = append(names, src.Name)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", s.ID, s.Time.Local().Format("2006-01-02 15:04"), s.Hostname, s.Stats.Files, cli.Size(s.Stats.Bytes), strings.Join(names, ", "))
		}
		return w.Flush()
	}
	if o.snapshot == "" || o.target == "" {
		return errors.New("restore needs --snapshot ID and --target DIR")
	}
	var snap *tree.Snapshot
	for _, s := range snaps {
		if s.ID == o.snapshot {
			snap = s
		}
	}
	if snap == nil {
		return fmt.Errorf("no snapshot %s in this repository; run: bb-server snapshots", o.snapshot)
	}
	st, err := restore.Run(context.Background(), func(id keys.ID) ([]byte, error) { return r.Get(id) }, restore.Options{
		Sources: snap.Sources, SourceOS: snap.OS, Include: o.include, Target: o.target,
		Warn: func(m string) { fmt.Fprintln(os.Stderr, "warning:", m) }})
	if err != nil {
		return err
	}
	fmt.Printf("restored %d files (%s) to %s; %d already up to date, %d warnings\n", st.Files, cli.Size(st.Bytes), o.target, st.Skipped, st.Warnings)
	if st.Warnings > 0 {
		os.Exit(1)
	}
	return nil
}
