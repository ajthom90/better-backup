package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestReportsAndRestoreBrowser backs up two machines with one policy,
// each with its own folder, and checks what the reports, the graphs
// and the restore browser say about it.
func TestReportsAndRestoreBrowser(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	srv, stop := startServer(t, server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond})
	defer stop()
	base := "https://" + srv.Addr()
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}

	ids := map[string]string{}
	for _, name := range []string{"one", "two"} {
		var tok api.EnrollToken
		e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
		dir := t.TempDir()
		cfg, err := agent.Enroll(dir, base, tok.Token, tok.Fingerprint, name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = cfg.AgentID
		ag, err := agent.New(dir, log)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { ag.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
	}

	src1, src2 := t.TempDir(), t.TempDir()
	write := func(dir, name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(src1, "keep.txt", "stays the same")
	write(src1, "edit.txt", "first")
	write(src1, "gone.txt", "will be removed")
	write(src1, "sub/big.bin", strings.Repeat("0123456789", 5000))
	write(src2, "other.txt", "the second machine")

	// One policy, two machines: the first by the policy's own list, the
	// second with a list of its own.
	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "documents", AgentIDs: []string{ids["one"], ids["two"]}, Paths: []string{src1},
		PerMachine: map[string]*api.Sources{ids["two"]: {Paths: []string{src2}}, "nobody": {Paths: []string{"/x"}}},
		Schedule:   api.Schedule{DailyAt: "23:59", Days: []int{1, 3}}, Enabled: true}, &pol)
	if len(pol.PerMachine) != 1 || pol.PerMachine[ids["two"]] == nil {
		t.Fatalf("per-machine sources: %+v", pol.PerMachine)
	}
	if err := admin.Do("POST", "/api/v1/policies", api.Policy{Name: "x", AgentIDs: []string{ids["one"], ids["two"]}, Paths: []string{src1},
		PerMachine: map[string]*api.Sources{ids["two"]: {}}}, nil); err == nil {
		t.Fatal("a machine with nothing to back up was accepted")
	}
	var pols []api.Policy
	e.do("GET", "/api/v1/policies", nil, &pols)
	if len(pols) != 1 || pols[0].NextRun == nil || (pols[0].NextRun.Weekday() != time.Monday && pols[0].NextRun.Weekday() != time.Wednesday) {
		t.Fatalf("next run of a Monday and Wednesday schedule: %+v", pols)
	}

	run := func(query string) {
		t.Helper()
		var first api.Job
		e.do("POST", "/api/v1/policies/"+pol.ID+"/run"+query, nil, &first)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var jobs []api.Job
			e.do("GET", "/api/v1/jobs?limit=50", nil, &jobs)
			busy := 0
			for _, j := range jobs {
				if j.Status == api.StatusQueued || j.Status == api.StatusRunning {
					busy++
				}
				if j.Status == api.StatusFailed {
					t.Fatalf("job failed: %s", j.Error)
				}
			}
			if busy == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the backups did not finish")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	run("")
	var snaps []tree.Snapshot
	e.do("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots after the first run", len(snaps))
	}
	for _, sn := range snaps {
		want := src1
		if sn.AgentID == ids["two"] {
			want = src2
		}
		if len(sn.Sources) != 1 || sn.Sources[0].Name != want {
			t.Fatalf("machine %s backed up %v, want %s", sn.Hostname, sn.Sources, want)
		}
	}
	firstRun := time.Now()
	time.Sleep(1100 * time.Millisecond) // snapshot times and file times are told apart by the second

	// The second machine is switched off; the first changes a file, adds
	// one and removes one.
	e.do("PUT", "/api/v1/policies/"+pol.ID+"/machines/"+ids["two"], map[string]bool{"off": true}, &pol)
	if len(pol.Off) != 1 {
		t.Fatalf("off: %v", pol.Off)
	}
	write(src1, "edit.txt", "second, and longer")
	write(src1, "new.txt", "added")
	os.Remove(filepath.Join(src1, "gone.txt"))
	run("")
	e.do("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 3 {
		t.Fatalf("%d snapshots: a machine that is switched off was backed up, or the other was not", len(snaps))
	}
	var h api.Health
	e.do("GET", "/api/v1/health", nil, &h)
	for _, p := range h.Policies {
		if p.AgentID == ids["two"] && p.State != "paused" {
			t.Fatalf("health of a machine that is switched off: %+v", p)
		}
	}

	// Reports.
	var reps []api.Report
	e.do("GET", "/api/v1/reports?agent_id="+ids["one"], nil, &reps)
	if len(reps) != 2 {
		t.Fatalf("%d reports for the first machine", len(reps))
	}
	last, first := reps[0], reps[1]
	if first.FilesNew != 4 || first.FilesModified != 0 || first.Files != 4 || first.Policy != "documents" || first.Agent != "one" {
		t.Fatalf("first run: %+v", first)
	}
	if last.FilesNew != 1 || last.FilesModified != 1 || last.FilesRemoved != 1 || last.Files != 4 || last.BytesChanged != int64(len("second, and longer")+len("added")) {
		t.Fatalf("second run: %+v", last)
	}
	if first.BytesRead < 50000 || first.BytesStored == 0 || first.Seconds <= 0 {
		t.Fatalf("what the first run read and stored: %+v", first)
	}
	e.do("GET", "/api/v1/reports?policy_id="+pol.ID+"&from="+time.Now().AddDate(0, 0, 1).Format("2006-01-02"), nil, &reps)
	if len(reps) != 0 {
		t.Fatalf("reports from tomorrow on: %d", len(reps))
	}
	e.do("GET", "/api/v1/reports?to="+time.Now().Format("2006-01-02"), nil, &reps)
	if len(reps) != 3 {
		t.Fatalf("reports through today: %d", len(reps))
	}
	raw, err := admin.Raw("GET", "/api/v1/reports?format=csv")
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 4 || !strings.HasPrefix(lines[0], "Started,Finished,Schedule,Machine") || !strings.Contains(lines[1], "documents,one,success") {
		t.Fatalf("the report as CSV:\n%s", raw)
	}

	// Graphs.
	var g api.Graphs
	e.do("GET", "/api/v1/graphs?days=7", nil, &g)
	today := g.Daily[len(g.Daily)-1]
	if len(g.Daily) != 7 || today.Backups != 3 || today.BytesRead < 50000 || len(g.Runs) != 3 || g.Sources == 0 || g.Stored == 0 {
		t.Fatalf("graphs: %+v", g)
	}

	// The restore browser: now, with what was removed, and as of the
	// time of the first run.
	names := func(l api.RestoreListing) string {
		var out []string
		for _, en := range l.Entries {
			n := en.Name
			if en.Removed {
				n += "(removed)"
			}
			out = append(out, n)
		}
		return strings.Join(out, " ")
	}
	browse := func(query string) api.RestoreListing {
		t.Helper()
		var l api.RestoreListing
		e.do("GET", "/api/v1/browse?agent_id="+ids["one"]+query, nil, &l)
		return l
	}
	if top := browse(""); len(top.Entries) != 1 || top.Entries[0].Name != src1 {
		t.Fatalf("the top of the restore browser: %+v", top)
	}
	p := "&path=" + url.QueryEscape(src1)
	if got := names(browse(p)); got != "sub edit.txt keep.txt new.txt" {
		t.Fatalf("now: %s", got)
	}
	if got := names(browse(p + "&removed=1")); got != "sub edit.txt gone.txt(removed) keep.txt new.txt" {
		t.Fatalf("now, with what was removed: %s", got)
	}
	then := browse(p + "&at=" + url.QueryEscape(firstRun.Add(time.Second).Format(time.RFC3339)))
	if got := names(then); got != "sub edit.txt gone.txt keep.txt" {
		t.Fatalf("as of the first run: %s", got)
	}
	if err := admin.Do("GET", "/api/v1/browse?agent_id="+ids["one"]+"&path="+url.QueryEscape(filepath.Join(src1, "nowhere")), nil, nil); err == nil {
		t.Fatal("a folder no backup holds was listed")
	}

	// Versions: the changed file has two, the unchanged one has one
	// that two snapshots share.
	var vs []api.FileVersion
	e.do("GET", "/api/v1/versions?agent_id="+ids["one"]+"&path="+url.QueryEscape(filepath.Join(src1, "edit.txt")), nil, &vs)
	if len(vs) != 2 || vs[0].Size != int64(len("second, and longer")) || vs[1].Size != 5 {
		t.Fatalf("versions of the changed file: %+v", vs)
	}
	e.do("GET", "/api/v1/versions?agent_id="+ids["one"]+"&path="+url.QueryEscape(filepath.Join(src1, "keep.txt")), nil, &vs)
	if len(vs) != 1 || vs[0].Snapshots != 2 {
		t.Fatalf("versions of the unchanged file: %+v", vs)
	}

	// The largest files, once the index of names has them.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var lf api.LargeFiles
		e.do("GET", "/api/v1/large-files?agent_id="+ids["one"], nil, &lf)
		if !lf.Building && len(lf.Files) == 4 {
			if lf.Files[0].Name != "big.bin" || lf.Files[0].Size != 50000 || lf.Files[0].Path != filepath.Join(src1, "sub", "big.bin") {
				t.Fatalf("the largest file: %+v", lf.Files[0])
			}
			e.do("GET", "/api/v1/large-files?q=EDIT", nil, &lf)
			if len(lf.Files) != 1 || lf.Files[0].Name != "edit.txt" {
				t.Fatalf("the largest files named edit: %+v", lf.Files)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the largest files never came: %+v", lf)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// One machine of a policy can be backed up by itself, switched off or not.
	run("?agent_id=" + ids["two"])
	e.do("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 4 {
		t.Fatalf("%d snapshots after backing up the one machine", len(snaps))
	}

	// The offsite copy says what waits for it.
	off := t.TempDir()
	e.do("PUT", "/api/v1/offsite", api.OffsiteSettings{Enabled: true, Kind: "folder", Folder: off}, nil)
	var st api.OffsiteStatus
	e.do("GET", "/api/v1/offsite", nil, &st)
	if st.Queue == nil || st.Queue.Files == 0 || st.Queue.Bytes == 0 || st.Queue.Oldest == nil {
		t.Fatalf("the queue before the first copy: %+v", st.Queue)
	}
}
