// bb is the better-backup admin command line. It talks to a server's
// admin API.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
  bb policy add --name NAME --agent AGENT [--path PATH ...]
        [--volume VOLUME ...]  (block-level image: C: or /dev/vg0/data)
        [--vm NAME ...]        (virtual machine on the agent's host: Hyper-V,
                                Proxmox VE or KVM with libvirt; '*' for all
                                on the host)
        [--hypervisor hyperv|libvirt|proxmox|vmware]  (default: whichever
                                the host is)
        [--vmware-server NAME] (with --hypervisor vmware: the server the
                                machines are on; see "bb vmware")
        [--vm-tracking on|check]  (read only what changed in each machine's
                                disks; check reads all and tests the tracking)
        [--system-state]       (Windows: registry, boot and system files,
                                Active Directory and SYSVOL on a domain controller)
        [--exclude PATTERN ...] [--every 24h | --daily-at 02:00]
        [--whole-machine]      (the system disk with its layout and every
                                partition, for bare-metal recovery)
        [--machine-disk DISK ...]  (with --whole-machine: these disks instead,
                                such as 0 and 1, or /dev/sda)
        [--truenas-config]     (TrueNAS SCALE: the system configuration)
        [--no-snapshot]  (read live files instead of a VSS or ZFS snapshot)
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
        [--overwrite if-changed|always|never] [--name NEW-VM-NAME] [--wait]
        [--disk NAME=TARGET ...]  (whole machine: put a disk of the backup,
                                such as disk0, onto a disk or into a file)
        [--vmware-server NAME] (for a VMware machine: where it goes, if not
                                the server its policy names; --target is
                                then a datastore)
                                         without --target, restores in place;
                                         a virtual machine is restored as a new
                                         machine with its files under --target
  bb restore SNAPSHOT --system-state-in-place [--no-registry] [--restart]
        [--authoritative-sysvol] [--authoritative DN ...] [--wait]
                                         put a Windows system state back over
                                         the machine it came from; finished by
                                         a restart. A domain controller must be
                                         in Directory Services Restore Mode.
                                         --authoritative makes restored Active
                                         Directory objects under DN win over
                                         the other domain controllers
  bb gc [--wait]                         reclaim space from forgotten snapshots
  bb verify [--wait]                     read and check every stored chunk
  bb alerts                              show the email alert settings
  bb alerts set [--smtp HOST[:PORT]] [--security starttls|tls|none]
        [--from ADDRESS] [--to ADDRESS ...] [--smtp-user USER]
        [--on-warning[=false]]           email failed and missed backups; the
                                         SMTP password is read from $BB_SMTP_PASSWORD
  bb alerts test                         send a test message
  bb alerts off                          stop sending alerts
  bb vmware                              list the VMware servers
  bb vmware add --name NAME --url ADDRESS --user USER
        [--thumbprint SHA256 | --insecure]
                                         add a vCenter Server or ESXi host, or
                                         change the one of that name; the
                                         password is read from $BB_VMWARE_PASSWORD
  bb vmware rm NAME
  bb offsite                             show the offsite copy and how it is doing
  bb offsite set --folder DIR [--daily-at 03:00]
                                         keep a copy on another disk or share
  bb offsite set --s3-endpoint URL --s3-bucket NAME [--s3-region REGION]
        [--s3-prefix FOLDER] [--s3-access-key KEY] [--daily-at 03:00]
                                         keep a copy in S3-compatible storage; the
                                         secret key is read from $BB_S3_SECRET_KEY
  bb offsite test                        check the copy's location can be used
  bb offsite run [--wait]                bring the copy up to date now
  bb offsite off                         stop updating the copy
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
	fSysSt   = fs.Bool("system-state", false, "")
	fMachine = fs.Bool("whole-machine", false, "")
	fTrueNAS = fs.Bool("truenas-config", false, "")
	fSysRest = fs.Bool("system-state-in-place", false, "")
	fNoReg   = fs.Bool("no-registry", false, "")
	fAuthSys = fs.Bool("authoritative-sysvol", false, "")
	fRestart = fs.Bool("restart", false, "")
	fAuthDN  cli.Multi
	fVMTrack = fs.String("vm-tracking", "", "")
	fHyper   = fs.String("hypervisor", "", "")
	fNoSnap  = fs.Bool("no-snapshot", false, "")
	fVMware  = fs.String("vmware-server", "", "")
	fURL     = fs.String("url", "", "")
	fVUser   = fs.String("user", "", "")
	fThumb   = fs.String("thumbprint", "", "")
	fInsec   = fs.Bool("insecure", false, "")
	fSMTP    = fs.String("smtp", "", "")
	fSec     = fs.String("security", "", "")
	fFrom    = fs.String("from", "", "")
	fUser    = fs.String("smtp-user", "", "")
	fOnWarn  = fs.Bool("on-warning", false, "")
	fFolder  = fs.String("folder", "", "")
	fS3End   = fs.String("s3-endpoint", "", "")
	fS3Buck  = fs.String("s3-bucket", "", "")
	fS3Reg   = fs.String("s3-region", "", "")
	fS3Pre   = fs.String("s3-prefix", "", "")
	fS3Key   = fs.String("s3-access-key", "", "")
	fS3VHost = fs.Bool("s3-virtual-host", false, "")
	fTo      cli.Multi
	fVols    cli.Multi
	fVMs     cli.Multi
	fMDisks  cli.Multi
	fDisks   cli.Multi
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
	fs.Var(&fTo, "to", "")
	fs.Var(&fVols, "volume", "")
	fs.Var(&fVMs, "vm", "")
	fs.Var(&fMDisks, "machine-disk", "")
	fs.Var(&fDisks, "disk", "")
	fs.Var(&fAuthDN, "authoritative", "")
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
			what := append(append(append([]string{}, p.Paths...), p.Volumes...), p.VMs...)
			if p.SystemState {
				what = append(what, "system state")
			}
			if p.BareMetal {
				what = append(what, "whole machine")
			}
			if p.TrueNASConfig {
				what = append(what, "TrueNAS configuration")
			}
			fmt.Fprintf(tabs, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.AgentID, sched, strings.Join(keep, ", "), strings.Join(what, ", "))
		}
	case "policy":
		switch sub {
		case "add":
			if *fName == "" || *fAgent == "" || len(fPaths)+len(fVols)+len(fVMs) == 0 && !*fSysSt && !*fMachine && !*fTrueNAS {
				cli.Fatal(errors.New("policy add needs --name, --agent and at least one --path, --volume or --vm, or --system-state, --whole-machine or --truenas-config"))
			}
			var p api.Policy
			do("POST", "/api/v1/policies", api.Policy{Name: *fName, AgentID: agentID(*fAgent), Paths: fPaths, Excludes: fExcl, Volumes: fVols, VMs: fVMs, Hypervisor: *fHyper, VMwareServer: vmwareID(*fVMware), VMTracking: *fVMTrack, SystemState: *fSysSt, TrueNASConfig: *fTrueNAS, BareMetal: *fMachine, MachineDisks: fMDisks,
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
		req := api.RestoreRequest{SnapshotID: pos[1], Include: fPaths, Target: *fTarget, Overwrite: *fOver, VMName: *fName, VMwareServer: vmwareID(*fVMware)}
		if *fSysRest {
			req.SystemState = &api.SystemStateRestore{Registry: !*fNoReg, AuthoritativeSYSVOL: *fAuthSys, AuthoritativeSubtrees: fAuthDN, Restart: *fRestart}
		} else if *fNoReg || *fAuthSys || len(fAuthDN) > 0 || *fRestart {
			cli.Fatal(errors.New("--no-registry, --authoritative-sysvol, --authoritative and --restart only go with --system-state-in-place"))
		}
		for _, d := range fDisks {
			name, to, ok := strings.Cut(d, "=")
			if !ok || name == "" || to == "" {
				cli.Fatal(errors.New("--disk takes NAME=TARGET, such as disk0=/dev/sdb"))
			}
			if req.Disks == nil {
				req.Disks = map[string]string{}
			}
			req.Disks[name] = to
		}
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
	case "alerts":
		var a api.AlertSettings
		switch sub {
		case "":
			do("GET", "/api/v1/alerts", nil, &a)
			printAlerts(&a)
		case "set":
			do("GET", "/api/v1/alerts", nil, &a)
			a.Enabled = true
			set := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
			if set["smtp"] {
				a.Host, a.Port = *fSMTP, 0
				if h, p, err := net.SplitHostPort(*fSMTP); err == nil {
					n, err := strconv.Atoi(p)
					if err != nil {
						cli.Fatal(fmt.Errorf("--smtp: %q is not a port number", p))
					}
					a.Host, a.Port = h, n
				}
			}
			if set["security"] {
				if !set["smtp"] || a.Port == 0 {
					a.Port = 0 // follow the new security setting's usual port
				}
				a.Security = *fSec
			}
			if set["from"] {
				a.From = *fFrom
			}
			if set["to"] {
				a.To = fTo
			}
			if set["smtp-user"] {
				a.Username = *fUser
			}
			if set["on-warning"] {
				a.OnWarning = *fOnWarn
			}
			if pw, ok := os.LookupEnv("BB_SMTP_PASSWORD"); ok {
				a.Password, a.PasswordSet = pw, false
			}
			do("PUT", "/api/v1/alerts", a, &a)
			printAlerts(&a)
			tabs.Flush()
			fmt.Println("\nSend a test message with: bb alerts test")
		case "off":
			do("GET", "/api/v1/alerts", nil, &a)
			a.Enabled = false
			do("PUT", "/api/v1/alerts", a, &a)
			fmt.Println("email alerts are off; the settings are kept")
		case "test":
			do("POST", "/api/v1/alerts/test", nil, nil)
			fmt.Println("test message sent")
		default:
			cli.Fatal(errors.New("usage: bb alerts [set|test|off]"))
		}
	case "vmware":
		var list []api.VMwareServer
		do("GET", "/api/v1/vmware-servers", nil, &list)
		switch sub {
		case "":
			if len(list) == 0 {
				fmt.Println("no VMware servers; add one with: bb vmware add")
				break
			}
			fmt.Fprintln(tabs, "ID\tNAME\tADDRESS\tSIGNS IN AS\tCERTIFICATE")
			for _, v := range list {
				cert := "checked against known authorities"
				if v.Insecure {
					cert = "not checked"
				} else if v.Thumbprint != "" {
					cert = "pinned to " + v.Thumbprint
				}
				fmt.Fprintf(tabs, "%s\t%s\t%s\t%s\t%s\n", v.ID, v.Name, v.URL, v.Username, cert)
			}
		case "add":
			if *fName == "" {
				cli.Fatal(errors.New("vmware add needs --name"))
			}
			v := api.VMwareServer{Name: *fName}
			method, path := "POST", "/api/v1/vmware-servers"
			for _, old := range list {
				if strings.EqualFold(old.Name, *fName) {
					v, method, path = old, "PUT", path+"/"+old.ID
				}
			}
			set := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
			if set["url"] {
				v.URL = *fURL
			}
			if set["user"] {
				v.Username = *fVUser
			}
			if set["thumbprint"] {
				v.Thumbprint = *fThumb
			}
			if set["insecure"] {
				v.Insecure = *fInsec
			}
			if pw, ok := os.LookupEnv("BB_VMWARE_PASSWORD"); ok {
				v.Password, v.PasswordSet = pw, false
			}
			do(method, path, v, &v)
			fmt.Printf("saved the VMware server %s (%s)\n", v.Name, v.ID)
		case "rm":
			need(pos, 3, "bb vmware rm NAME")
			do("DELETE", "/api/v1/vmware-servers/"+vmwareID(pos[2]), nil, nil)
			fmt.Println("removed")
		default:
			cli.Fatal(errors.New("usage: bb vmware [add|rm]"))
		}
	case "offsite":
		var st api.OffsiteStatus
		switch sub {
		case "":
			do("GET", "/api/v1/offsite", nil, &st)
			printOffsite(&st)
		case "set":
			do("GET", "/api/v1/offsite", nil, &st)
			o := st.OffsiteSettings
			o.Enabled = true
			set := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
			if set["folder"] {
				o.Kind, o.Folder = "folder", *fFolder
			}
			if set["s3-endpoint"] {
				o.Kind, o.Endpoint = "s3", *fS3End
			}
			if set["s3-bucket"] {
				o.Kind, o.Bucket = "s3", *fS3Buck
			}
			if set["s3-region"] {
				o.Region = *fS3Reg
			}
			if set["s3-prefix"] {
				o.Prefix = *fS3Pre
			}
			if set["s3-access-key"] {
				o.AccessKey = *fS3Key
			}
			if set["s3-virtual-host"] {
				o.VirtualHost = *fS3VHost
			}
			if set["daily-at"] {
				o.DailyAt = *fDaily
			} else if o.DailyAt == "" && st.Saved.IsZero() {
				o.DailyAt = "03:00"
			}
			if k, ok := os.LookupEnv("BB_S3_SECRET_KEY"); ok {
				o.SecretKey, o.SecretKeySet = k, false
			}
			do("PUT", "/api/v1/offsite", o, &st)
			printOffsite(&st)
			tabs.Flush()
			fmt.Println("\nCheck it with: bb offsite test    Start the first copy with: bb offsite run")
		case "off":
			do("GET", "/api/v1/offsite", nil, &st)
			o := st.OffsiteSettings
			o.Enabled = false
			do("PUT", "/api/v1/offsite", o, &st)
			fmt.Println("the offsite copy is no longer updated; the settings and the copy are kept")
		case "test":
			var t api.OffsiteTest
			do("POST", "/api/v1/offsite/test", nil, &t)
			fmt.Printf("the location can be read and written; it holds %d files (%s)\n", t.Objects, cli.Size(t.Bytes))
		case "run":
			var j api.Job
			do("POST", "/api/v1/offsite/run", nil, &j)
			follow(&j)
		default:
			cli.Fatal(errors.New("usage: bb offsite [set|test|run|off]"))
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
}

// vmwareID finds a VMware server by id or name.
func vmwareID(ref string) string {
	if ref == "" {
		return ""
	}
	var list []api.VMwareServer
	do("GET", "/api/v1/vmware-servers", nil, &list)
	for _, v := range list {
		if v.ID == ref || strings.EqualFold(v.Name, ref) {
			return v.ID
		}
	}
	cli.Fatal(fmt.Errorf("no VMware server %q; list them with: bb vmware", ref))
	return ""
}

func printOffsite(st *api.OffsiteStatus) {
	if st.Kind == "" {
		fmt.Println("no offsite copy is set up; see: bb help")
		return
	}
	yes := map[bool]string{true: "yes", false: "no"}
	fmt.Fprintf(tabs, "Updating the copy\t%s\n", yes[st.Enabled])
	if st.Kind == "folder" {
		fmt.Fprintf(tabs, "Folder\t%s\n", st.Folder)
	} else {
		where := st.Bucket
		if st.Prefix != "" {
			where += "/" + st.Prefix
		}
		fmt.Fprintf(tabs, "Bucket\t%s at %s\nAccess key\t%s (secret key saved: %s)\n", where, st.Endpoint, st.AccessKey, yes[st.SecretKeySet])
	}
	when := "only when started by hand"
	if st.DailyAt != "" {
		when = "every day at " + st.DailyAt
	}
	fmt.Fprintf(tabs, "Schedule\t%s\n", when)
	last := "never"
	if st.LastSuccess != nil {
		last = st.LastSuccess.Local().Format(timeForm)
	}
	fmt.Fprintf(tabs, "Last complete copy\t%s\n", last)
	if j := st.LastJob; j != nil && j.Status == api.StatusFailed {
		fmt.Fprintf(tabs, "Last run failed\t%s\n", j.Error)
	}
}

func printAlerts(a *api.AlertSettings) {
	if a.Host == "" {
		fmt.Println("email alerts are not set up; see: bb help")
		return
	}
	yes := map[bool]string{true: "yes", false: "no"}
	fmt.Fprintf(tabs, "Sending alerts\t%s\nMail server\t%s:%d (%s)\n", yes[a.Enabled], a.Host, a.Port, a.Security)
	if a.Username != "" {
		fmt.Fprintf(tabs, "Signs in as\t%s (password saved: %s)\n", a.Username, yes[a.PasswordSet])
	}
	fmt.Fprintf(tabs, "From\t%s\nTo\t%s\nBackups with warnings\t%s\n", a.From, strings.Join(a.To, ", "), yes[a.OnWarning])
}

func sub2(pos []string) string {
	if len(pos) > 2 {
		return pos[2]
	}
	return ""
}
