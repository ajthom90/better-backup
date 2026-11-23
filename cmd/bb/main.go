// bb is the better-backup admin command line. It talks to a server's
// admin API.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/cli"
	"github.com/ajthom90/better-backup/internal/paths"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

const usage = `bb manages a better-backup server.

Usage:
  bb status                              server and repository summary
  bb token create [--ttl 24h] [--uses 1] make an enrollment token for new agents
  bb agents                              list agents
  bb agent revoke AGENT                  block an agent
  bb policies                            list backup policies
  bb policy add --name NAME --agent AGENT --path PATH [--path ...]
        [--exclude PATTERN ...] [--every 24h | --daily-at 02:00]
        [--no-snapshot]  (Windows: read live files instead of a VSS snapshot)
        [--keep-last N] [--keep-daily N] [--keep-weekly N]
        [--keep-monthly N] [--keep-yearly N]
  bb policy run POLICY [--wait]          back up now
  bb policy rm POLICY
  bb jobs [--limit N]                    recent jobs
  bb job JOB                             one job in detail
  bb snapshots [--agent AGENT]           list snapshots
  bb snapshot rm SNAPSHOT                forget a snapshot
  bb ls SNAPSHOT [PATH]                  browse a snapshot
  bb restore SNAPSHOT [--path PATH ...] [--target DIR] [--agent AGENT]
        [--overwrite if-changed|always|never] [--wait]
                                         without --target, restores in place
  bb gc [--wait]                         reclaim space from forgotten snapshots
  bb verify [--wait]                     read and check every stored chunk
  bb login --server URL --token TOKEN --fingerprint FP
                                         save connection settings for this user
  bb version

AGENT and POLICY may be an id or a name.

On the server machine bb needs no settings: it reads them from the server's
data directory. Elsewhere use "bb login", or --server/--token/--fingerprint,
or $BB_SERVER, $BB_TOKEN and $BB_FINGERPRINT.
`

type conn struct {
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

func cliConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "better-backup", "cli.json")
}

var (
	fs       = flag.NewFlagSet("bb", flag.ExitOnError)
	fServer  = fs.String("server", os.Getenv("BB_SERVER"), "")
	fToken   = fs.String("token", os.Getenv("BB_TOKEN"), "")
	fFP      = fs.String("fingerprint", os.Getenv("BB_FINGERPRINT"), "")
	fTTL     = fs.String("ttl", "24h", "")
	fUses    = fs.Int("uses", 1, "")
	fName    = fs.String("name", "", "")
	fAgent   = fs.String("agent", "", "")
	fEvery   = fs.String("every", "", "")
	fDaily   = fs.String("daily-at", "", "")
	fLimit   = fs.Int("limit", 20, "")
	fTarget  = fs.String("target", "", "")
	fOver    = fs.String("overwrite", "", "")
	fWait    = fs.Bool("wait", false, "")
	fNoSnap  = fs.Bool("no-snapshot", false, "")
	fPaths   cli.Multi
	fExcl    cli.Multi
	fKeep    api.Retention
	clientV  *adminclient.Client
	tabs     = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	timeForm = "2006-01-02 15:04"
)

func client() *adminclient.Client {
	if clientV != nil {
		return clientV
	}
	c := conn{*fServer, *fToken, *fFP}
	if c.URL == "" || c.Token == "" || c.Fingerprint == "" {
		// Saved login first, then the local server's own admin file.
		for _, p := range []string{cliConfigPath(), filepath.Join(paths.ServerData(), "admin.json")} {
			var f conn
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &f) == nil && f.URL != "" {
				if c.URL == "" {
					c.URL = f.URL
				}
				if c.Token == "" {
					c.Token = f.Token
				}
				if c.Fingerprint == "" {
					c.Fingerprint = f.Fingerprint
				}
				break
			}
		}
	}
	if c.URL == "" || c.Token == "" || c.Fingerprint == "" {
		cli.Fatal(errors.New("no server configured: run \"bb login\", or run bb on the server machine as a user who can read its data directory"))
	}
	cl, err := adminclient.New(c.URL, c.Token, c.Fingerprint)
	if err != nil {
		cli.Fatal(err)
	}
	clientV = cl
	return cl
}

func do(method, path string, in, out any) {
	if err := client().Do(method, path, in, out); err != nil {
		cli.Fatal(err)
	}
}

func need(pos []string, n int, what string) {
	if len(pos) < n {
		cli.Fatal(fmt.Errorf("missing argument: %s (see: bb help)", what))
	}
}

func agentID(ref string) string {
	var as []api.Agent
	do("GET", "/api/v1/agents", nil, &as)
	var match []api.Agent
	for _, a := range as {
		if a.ID == ref {
			return a.ID
		}
		if strings.EqualFold(a.Name, ref) && !a.Revoked {
			match = append(match, a)
		}
	}
	if len(match) != 1 {
		cli.Fatal(fmt.Errorf("%d active agents are named %q; use the id from: bb agents", len(match), ref))
	}
	return match[0].ID
}

func policyID(ref string) string {
	var ps []api.Policy
	do("GET", "/api/v1/policies", nil, &ps)
	var match []string
	for _, p := range ps {
		if p.ID == ref {
			return p.ID
		}
		if strings.EqualFold(p.Name, ref) {
			match = append(match, p.ID)
		}
	}
	if len(match) != 1 {
		cli.Fatal(fmt.Errorf("%d policies are named %q; use the id from: bb policies", len(match), ref))
	}
	return match[0]
}

func age(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// follow waits for a job when --wait is set, and exits non-zero if it
// did not succeed.
func follow(j *api.Job) {
	if !*fWait {
		fmt.Printf("queued %s job %s; follow it with: bb job %s\n", j.Type, j.ID, j.ID)
		return
	}
	for j.Status == api.StatusQueued || j.Status == api.StatusRunning {
		time.Sleep(time.Second)
		do("GET", "/api/v1/jobs/"+j.ID, nil, j)
	}
	printJob(j)
	if j.Status == api.StatusFailed {
		os.Exit(1)
	}
}

func printJob(j *api.Job) {
	fmt.Printf("job %s: %s %s\n", j.ID, j.Type, j.Status)
	if j.Error != "" {
		fmt.Println("error:", j.Error)
	}
	if j.SnapshotID != "" {
		fmt.Println("snapshot:", j.SnapshotID)
	}
	if j.Started != nil && j.Finished != nil {
		fmt.Println("took:", j.Finished.Sub(*j.Started).Round(time.Second))
	}
	if len(j.Result) > 0 {
		fmt.Println("result:", string(j.Result))
	} else if len(j.Progress) > 0 {
		fmt.Println("progress:", string(j.Progress))
	}
	for _, w := range j.Warnings {
		fmt.Println("warning:", w)
	}
}

func main() {
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Var(&fPaths, "path", "")
	fs.Var(&fExcl, "exclude", "")
	fs.IntVar(&fKeep.KeepLast, "keep-last", 0, "")
	fs.IntVar(&fKeep.KeepDaily, "keep-daily", 0, "")
	fs.IntVar(&fKeep.KeepWeekly, "keep-weekly", 0, "")
	fs.IntVar(&fKeep.KeepMonthly, "keep-monthly", 0, "")
	fs.IntVar(&fKeep.KeepYearly, "keep-yearly", 0, "")
	pos := cli.Parse(fs, os.Args[1:])
	if len(pos) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	sub := ""
	if len(pos) > 1 {
		sub = pos[1]
	}
	defer tabs.Flush()

	switch pos[0] {
	case "help":
		fmt.Print(usage)
	case "version":
		fmt.Println("bb", server.Version, runtime.GOOS+"/"+runtime.GOARCH)
	case "login":
		if *fServer == "" || *fToken == "" || *fFP == "" {
			cli.Fatal(errors.New("login needs --server, --token and --fingerprint"))
		}
		var st api.Status
		do("GET", "/api/v1/status", nil, &st)
		p := cliConfigPath()
		b, _ := json.MarshalIndent(conn{*fServer, *fToken, *fFP}, "", "  ")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			cli.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			cli.Fatal(err)
		}
		fmt.Printf("connected to better-backup %s; settings saved to %s\n", st.Version, p)
	case "status":
		var st api.Status
		do("GET", "/api/v1/status", nil, &st)
		fmt.Fprintf(tabs, "Version\t%s\nAgents\t%d\nSnapshots\t%d\nRunning jobs\t%d\n", st.Version, st.Agents, st.Snapshots, st.RunningJobs)
		fmt.Fprintf(tabs, "Stored on disk\t%s in %d pack files\n", cli.Size(st.StoredBytes), st.Packs)
		fmt.Fprintf(tabs, "Unique data\t%s in %d chunks\n", cli.Size(st.RawBytes), st.Chunks)
		fmt.Fprintf(tabs, "CA fingerprint\t%s\n", st.Fingerprint)
	case "token":
		var t api.EnrollToken
		do("POST", "/api/v1/enroll-tokens", map[string]any{"ttl": *fTTL, "uses": *fUses}, &t)
		fmt.Printf("Token valid until %s for %d enrollment(s). On the machine to back up, run:\n\n", t.Expires.Local().Format(timeForm), t.Uses)
		fmt.Printf("  bb-agent enroll --server https://THIS-SERVER:8443 --token %s --fingerprint %s\n\n", t.Token, t.Fingerprint)
		fmt.Println("Replace THIS-SERVER with the address the machine reaches this server by.")
	case "agents":
		var as []api.Agent
		do("GET", "/api/v1/agents", nil, &as)
		fmt.Fprintln(tabs, "ID\tNAME\tHOST\tOS\tVERSION\tSTATE\tLAST SEEN")
		for _, a := range as {
			state := "offline"
			if a.Revoked {
				state = "revoked"
			} else if a.Online {
				state = "online"
			}
			fmt.Fprintf(tabs, "%s\t%s\t%s\t%s/%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Hostname, a.OS, a.Arch, a.Version, state, age(a.LastSeen))
		}
	case "agent":
		need(pos, 3, "bb agent revoke AGENT")
		do("DELETE", "/api/v1/agents/"+agentID(pos[2]), nil, nil)
		fmt.Println("agent revoked")
	case "policies":
		var ps []api.Policy
		do("GET", "/api/v1/policies", nil, &ps)
		fmt.Fprintln(tabs, "ID\tNAME\tAGENT\tSCHEDULE\tKEEP\tPATHS")
		for _, p := range ps {
			sched := "manual"
			if p.Schedule.Interval != "" {
				sched = "every " + p.Schedule.Interval
			} else if p.Schedule.DailyAt != "" {
				sched = "daily at " + p.Schedule.DailyAt
			}
			if !p.Enabled {
				sched += " (disabled)"
			}
			var keep []string
			for _, k := range []struct {
				n int
				s string
			}{{p.Retention.KeepLast, "last"}, {p.Retention.KeepDaily, "daily"}, {p.Retention.KeepWeekly, "weekly"}, {p.Retention.KeepMonthly, "monthly"}, {p.Retention.KeepYearly, "yearly"}} {
				if k.n > 0 {
					keep = append(keep, fmt.Sprintf("%d %s", k.n, k.s))
				}
			}
			if len(keep) == 0 {
				keep = []string{"all"}
			}
			fmt.Fprintf(tabs, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.AgentID, sched, strings.Join(keep, ", "), strings.Join(p.Paths, ", "))
		}
	case "policy":
		switch sub {
		case "add":
			if *fName == "" || *fAgent == "" || len(fPaths) == 0 {
				cli.Fatal(errors.New("policy add needs --name, --agent and at least one --path"))
			}
			var p api.Policy
			do("POST", "/api/v1/policies", api.Policy{Name: *fName, AgentID: agentID(*fAgent), Paths: fPaths, Excludes: fExcl,
				Schedule: api.Schedule{Interval: *fEvery, DailyAt: *fDaily}, Retention: fKeep, NoSnapshot: *fNoSnap, Enabled: true}, &p)
			fmt.Printf("created policy %s (%s)\n", p.Name, p.ID)
		case "run":
			need(pos, 3, "bb policy run POLICY")
			var j api.Job
			do("POST", "/api/v1/policies/"+policyID(pos[2])+"/run", nil, &j)
			follow(&j)
		case "rm":
			need(pos, 3, "bb policy rm POLICY")
			do("DELETE", "/api/v1/policies/"+policyID(pos[2]), nil, nil)
			fmt.Println("policy removed; its snapshots are kept")
		default:
			cli.Fatal(errors.New("usage: bb policy add|run|rm"))
		}
	case "jobs":
		var js []api.Job
		do("GET", fmt.Sprintf("/api/v1/jobs?limit=%d", *fLimit), nil, &js)
		fmt.Fprintln(tabs, "ID\tTYPE\tSTATUS\tAGENT\tCREATED\tDETAIL")
		for _, j := range js {
			detail := j.Error
			if detail == "" && j.SnapshotID != "" {
				detail = "snapshot " + j.SnapshotID
			}
			fmt.Fprintf(tabs, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Type, j.Status, j.AgentID, j.Created.Local().Format(timeForm), detail)
		}
	case "job":
		need(pos, 2, "bb job JOB")
		var j api.Job
		do("GET", "/api/v1/jobs/"+pos[1], nil, &j)
		printJob(&j)
	case "snapshots":
		q := ""
		if *fAgent != "" {
			q = "?agent_id=" + agentID(*fAgent)
		}
		var ss []tree.Snapshot
		do("GET", "/api/v1/snapshots"+q, nil, &ss)
		fmt.Fprintln(tabs, "SNAPSHOT\tTIME\tHOST\tFILES\tSIZE\tNEW DATA\tPATHS")
		for _, s := range ss {
			var names []string
			for _, src := range s.Sources {
				names = append(names, src.Name)
			}
			fmt.Fprintf(tabs, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, s.Time.Local().Format(timeForm), s.Hostname, s.Stats.Files, cli.Size(s.Stats.Bytes), cli.Size(s.Stats.BytesUploaded), strings.Join(names, ", "))
		}
	case "snapshot":
		need(pos, 3, "bb snapshot rm SNAPSHOT")
		do("DELETE", "/api/v1/snapshots/"+pos[2], nil, nil)
		fmt.Println("snapshot forgotten; run \"bb gc\" to reclaim its space now")
	case "ls":
		need(pos, 2, "bb ls SNAPSHOT [PATH]")
		var es []api.ListEntry
		do("GET", "/api/v1/snapshots/"+pos[1]+"/ls?path="+url.QueryEscape(sub2(pos)), nil, &es)
		for _, e := range es {
			name := e.Name
			if e.Type == tree.TypeDir {
				name += "/"
			}
			fmt.Fprintf(tabs, "%s\t%s\t%s\t%s\n", e.Type, cli.Size(e.Size), e.ModTime.Local().Format(timeForm), name)
		}
	case "restore":
		need(pos, 2, "bb restore SNAPSHOT")
		req := api.RestoreRequest{SnapshotID: pos[1], Include: fPaths, Target: *fTarget, Overwrite: *fOver}
		if *fAgent != "" {
			req.AgentID = agentID(*fAgent)
		}
		var j api.Job
		do("POST", "/api/v1/restores", req, &j)
		follow(&j)
	case "gc", "verify":
		var j api.Job
		do("POST", "/api/v1/repo/"+pos[0], nil, &j)
		follow(&j)
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func sub2(pos []string) string {
	if len(pos) > 2 {
		return pos[2]
	}
	return ""
}
