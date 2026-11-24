package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// protected is a server with an administrator, an operator, one agent
// and a webhook that collects the alerts.
type protected struct {
	t                *testing.T
	srv              *server.Server
	token, admin, op *web
	agentID          string
	mu               sync.Mutex
	alerts           []string
}

const adminPass, opPass = "correct horse battery", "operator password"

func newProtected(t *testing.T, cfg server.Config) *protected {
	t.Helper()
	cfg.DataDir, cfg.Listen, cfg.Tick, cfg.Log = t.TempDir(), "127.0.0.1:0", 50*time.Millisecond, quiet
	srv, _ := startServer(t, cfg)
	p := &protected{t: t, srv: srv}
	var af server.AdminFile
	readJSON(t, filepath.Join(cfg.DataDir, "admin.json"), &af)
	w := newWeb(t, srv.Addr())
	p.token = w.as(af.Token)
	w.must("POST", "/api/v1/setup", api.SetupRequest{Name: "andrew", Password: adminPass}, nil)
	p.admin = w.login("andrew", adminPass)
	p.admin.must("POST", "/api/v1/users", api.User{Name: "olive", Role: api.RoleOperator, Password: opPass}, nil)
	p.op = w.login("olive", opPass)

	hook := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		json.Unmarshal(b, &m)
		p.mu.Lock()
		p.alerts = append(p.alerts, m["subject"]+"\n"+m["text"])
		p.mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	p.admin.must("PUT", "/api/v1/alerts", api.AlertSettings{Webhook: hook.URL}, nil)

	var tok api.EnrollToken
	p.admin.must("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, w.base, tok.Token, tok.Fingerprint, "fileserver"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(adir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ag.Run(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	var agents []api.Agent
	p.admin.must("GET", "/api/v1/agents", nil, &agents)
	p.agentID = agents[0].ID
	return p
}

// alerted waits for an alert whose subject or text has every part.
func (p *protected) alerted(parts ...string) {
	p.t.Helper()
	waitFor(p.t, "an alert with "+strings.Join(parts, ", "), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, a := range p.alerts {
			ok := true
			for _, part := range parts {
				ok = ok && strings.Contains(a, part)
			}
			if ok {
				return true
			}
		}
		return false
	})
}

// backup runs a policy and returns the finished job, whatever its
// status.
func (p *protected) backup(policy string) *api.Job {
	p.t.Helper()
	var j api.Job
	p.op.must("POST", "/api/v1/policies/"+policy+"/run", nil, &j)
	waitFor(p.t, "backup "+j.ID, func() bool {
		p.op.must("GET", "/api/v1/jobs/"+j.ID, nil, &j)
		return j.Status != api.StatusQueued && j.Status != api.StatusRunning
	})
	if j.Status == api.StatusFailed {
		p.t.Fatalf("backup: %s", j.Error)
	}
	return &j
}

func (p *protected) live() []string {
	var snaps []struct {
		ID string `json:"id"`
	}
	p.op.must("GET", "/api/v1/snapshots", nil, &snaps)
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	return ids
}

func (p *protected) deleted() []api.DeletedSnapshot {
	var d []api.DeletedSnapshot
	p.op.must("GET", "/api/v1/deleted-snapshots", nil, &d)
	return d
}

func (p *protected) status() api.ProtectionStatus {
	var st api.ProtectionStatus
	p.op.must("GET", "/api/v1/protection", nil, &st)
	return st
}

// office writes folders of documents that compress well.
func office(t *testing.T, root string, folders, each int) {
	for d := 0; d < folders; d++ {
		for f := 0; f < each; f++ {
			write(t, filepath.Join(root, fmt.Sprintf("dept%02d", d), fmt.Sprintf("report-%03d.docx", f)),
				[]byte(strings.Repeat(fmt.Sprintf("Quarterly figures for department %d, report %d. ", d, f), 40)))
		}
	}
}

// TestProtection covers the delay before a deleted snapshot is gone, who
// may delete, the retention lock, and that weakening any of it waits.
func TestProtection(t *testing.T) {
	p := newProtected(t, server.Config{})
	src := filepath.Join(t.TempDir(), "share")
	office(t, src, 4, 30)
	var pol api.Policy
	p.op.must("POST", "/api/v1/policies", api.Policy{Name: "share", AgentID: p.agentID, Paths: []string{src}, Retention: api.Retention{KeepLast: 1}, Enabled: true}, &pol)

	if st := p.status(); st.LockDays != 0 || st.DeleteDelayDays != 7 || !st.Detect || st.WaitHours != 72 {
		t.Fatalf("defaults: %+v", st)
	}

	// The retention rule keeps one snapshot. The one it forgets is set
	// aside, not gone.
	j1 := p.backup(pol.ID)
	write(t, filepath.Join(src, "dept00", "new.docx"), []byte("one more"))
	j2 := p.backup(pol.ID)
	if live := p.live(); len(live) != 1 || live[0] != j2.SnapshotID {
		t.Fatalf("live snapshots after retention: %v", live)
	}
	d := p.deleted()
	if len(d) != 1 || d[0].ID != j1.SnapshotID || d[0].By != "retention" || d[0].Policy != "share" || d[0].Purge.Sub(d[0].Deleted) != 7*24*time.Hour {
		t.Fatalf("deleted snapshots: %+v", d)
	}
	// Reclaiming space leaves its data alone.
	var gc api.Job
	p.op.must("POST", "/api/v1/repo/gc", nil, &gc)
	waitJob(t, p.op, gc.ID)
	// An operator brings it back, and it restores.
	p.op.refused(404, "POST", "/api/v1/deleted-snapshots/nope/restore", nil)
	p.op.must("POST", "/api/v1/deleted-snapshots/"+j1.SnapshotID+"/restore", nil, nil)
	if len(p.live()) != 2 || len(p.deleted()) != 0 {
		t.Fatalf("after bringing it back: live %v, deleted %+v", p.live(), p.deleted())
	}
	out := t.TempDir()
	var rj api.Job
	p.op.must("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j1.SnapshotID, Target: out}, &rj)
	waitJob(t, p.op, rj.ID)

	// Deleting by hand: an administrator, with their password.
	path := "/api/v1/snapshots/" + j1.SnapshotID
	p.op.refused(403, "DELETE", path, nil)
	p.admin.refused(403, "DELETE", path, nil)
	bad := *p.admin
	bad.password = "not the password"
	bad.refused(403, "DELETE", path, nil)
	sure := *p.admin
	sure.password = adminPass
	sure.must("DELETE", path, nil, nil)
	if d := p.deleted(); len(d) != 1 || d[0].By != "andrew" {
		t.Fatalf("deleted by hand: %+v", d)
	}
	p.alerted("A snapshot was deleted", "andrew", j1.SnapshotID, "7 days")
	// The command line's token is an administrator without a password.
	p.token.must("POST", "/api/v1/deleted-snapshots/"+j1.SnapshotID+"/restore", nil, nil)
	p.token.must("DELETE", path, nil, nil)

	// When its time is up it is gone, and so is its data.
	p.srv.ExpireDeleted()
	waitFor(t, "the deleted snapshot to be given up", func() bool { return len(p.deleted()) == 0 })
	p.op.refused(404, "GET", path, nil)
	p.op.must("POST", "/api/v1/repo/gc", nil, &gc)
	waitJob(t, p.op, gc.ID)

	// The retention lock: stronger settings apply at once.
	p.op.refused(403, "PUT", "/api/v1/protection", api.ProtectionRequest{ProtectionSettings: api.ProtectionSettings{LockDays: 30}})
	p.admin.refused(403, "PUT", "/api/v1/protection", api.ProtectionRequest{ProtectionSettings: api.ProtectionSettings{LockDays: 30, DeleteDelayDays: 7, Detect: true}})
	p.admin.refused(400, "PUT", "/api/v1/protection", api.ProtectionRequest{ProtectionSettings: api.ProtectionSettings{LockDays: -1}, Password: adminPass})
	var st api.ProtectionStatus
	p.admin.must("PUT", "/api/v1/protection", api.ProtectionRequest{ProtectionSettings: api.ProtectionSettings{LockDays: 30, DeleteDelayDays: 14, Detect: true}, Password: adminPass}, &st)
	if st.LockDays != 30 || st.DeleteDelayDays != 14 || st.Pending != nil {
		t.Fatalf("stronger settings: %+v", st)
	}
	if msg := sure.refused(409, "DELETE", "/api/v1/snapshots/"+j2.SnapshotID, nil); !strings.Contains(msg, "locked until") {
		t.Fatalf("deleting a locked snapshot: %s", msg)
	}
	// Retention cannot forget a locked snapshot either.
	write(t, filepath.Join(src, "dept00", "newer.docx"), []byte("and another"))
	p.backup(pol.ID)
	if len(p.live()) != 2 || len(p.deleted()) != 0 {
		t.Fatalf("retention under the lock: live %v, deleted %+v", p.live(), p.deleted())
	}

	// Weaker settings wait, and everyone is told.
	p.admin.must("PUT", "/api/v1/protection", api.ProtectionRequest{ProtectionSettings: api.ProtectionSettings{LockDays: 60, DeleteDelayDays: 0, Detect: false}, Password: adminPass}, &st)
	if st.LockDays != 60 || st.DeleteDelayDays != 14 || !st.Detect || st.Pending == nil || st.Pending.DeleteDelayDays != 0 || st.Pending.Detect ||
		st.PendingBy != "andrew" || time.Until(*st.PendingAt) < 71*time.Hour {
		t.Fatalf("weaker settings: %+v", st)
	}
	p.alerted("going to be weakened", "andrew", "deleted snapshots are gone at once")
	var h api.Health
	p.op.must("GET", "/api/v1/health", nil, &h)
	if !strings.Contains(fmt.Sprint(h.Issues), "will be weakened") {
		t.Fatalf("health does not mention the waiting change: %+v", h.Issues)
	}
	p.op.refused(403, "DELETE", "/api/v1/protection/pending", nil)
	st = api.ProtectionStatus{}
	p.admin.must("DELETE", "/api/v1/protection/pending", nil, &st)
	if st.Pending != nil || st.DeleteDelayDays != 14 {
		t.Fatalf("after cancelling: %+v", st)
	}
	p.alerted("weakening of the ransomware protection was cancelled")
	p.admin.refused(404, "DELETE", "/api/v1/protection/pending", nil)

	// Whoever got the alerts hears when they are switched off.
	p.admin.must("PUT", "/api/v1/alerts", api.AlertSettings{}, nil)
	p.alerted("alert settings were changed", "andrew")

	var log []api.AuditEntry
	p.admin.must("GET", "/api/v1/audit", nil, &log)
	all := fmt.Sprint(log)
	for _, want := range []string{"Deleted a snapshot", "Brought back a deleted snapshot", "Changed the ransomware protection", "Cancelled the weakening"} {
		if !strings.Contains(all, want) {
			t.Errorf("the activity log lacks %q", want)
		}
	}
}

// TestProtectionOff: with no delay a deleted snapshot is gone at once,
// and a weakening change that waited takes effect when its time comes.
func TestProtectionOff(t *testing.T) {
	p := newProtected(t, server.Config{ProtectionWait: 300 * time.Millisecond})
	src := filepath.Join(t.TempDir(), "share")
	office(t, src, 1, 5)
	var pol api.Policy
	p.op.must("POST", "/api/v1/policies", api.Policy{Name: "share", AgentID: p.agentID, Paths: []string{src}, Enabled: true}, &pol)
	j := p.backup(pol.ID)
	var st api.ProtectionStatus
	p.admin.must("PUT", "/api/v1/protection", api.ProtectionRequest{ProtectionSettings: api.ProtectionSettings{}, Password: adminPass}, &st)
	if st.DeleteDelayDays != 7 || st.Pending == nil {
		t.Fatalf("at first: %+v", st)
	}
	waitFor(t, "the waiting change to apply", func() bool { st := p.status(); return st.Pending == nil && st.DeleteDelayDays == 0 && !st.Detect })
	p.alerted("The ransomware protection was weakened")
	p.token.must("DELETE", "/api/v1/snapshots/"+j.SnapshotID, nil, nil)
	if len(p.live()) != 0 || len(p.deleted()) != 0 {
		t.Fatalf("live %v, deleted %+v", p.live(), p.deleted())
	}
	p.alerted("A snapshot was deleted", "gone for good")
}

// encrypt does to a folder what ransomware does: every file becomes
// random bytes under its old name plus an ending, and a note is left.
func encrypt(t *testing.T, root, ending string) {
	seed := int64(100)
	filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if fi.IsDir() {
			write(t, filepath.Join(path, "HOW_TO_DECRYPT_FILES.txt"), []byte("Your files are encrypted. Pay us."))
			return nil
		}
		if strings.HasPrefix(fi.Name(), "HOW_TO") {
			return nil
		}
		seed++
		write(t, path+ending, rnd(seed, int(fi.Size())))
		return os.Remove(path)
	})
}

// TestRansomwareDetection: a backup of files that ransomware has been at
// is kept and marked, people are told, and nothing older is forgotten
// until an administrator has looked.
func TestRansomwareDetection(t *testing.T) {
	p := newProtected(t, server.Config{})
	src := filepath.Join(t.TempDir(), "share")
	office(t, src, 8, 25)
	var pol api.Policy
	p.op.must("POST", "/api/v1/policies", api.Policy{Name: "share", AgentID: p.agentID, Paths: []string{src}, Retention: api.Retention{KeepLast: 1}, Enabled: true}, &pol)

	// Ordinary days: a few files change, some are added and removed.
	p.backup(pol.ID)
	for i := 0; i < 10; i++ {
		write(t, filepath.Join(src, "dept01", fmt.Sprintf("report-%03d.docx", i)), []byte(strings.Repeat("revised ", 100)))
		write(t, filepath.Join(src, "dept02", fmt.Sprintf("minutes-%d.docx", i)), []byte("minutes"))
		os.Remove(filepath.Join(src, "dept03", fmt.Sprintf("report-%03d.docx", i)))
	}
	good := p.backup(pol.ID)
	if good.Status != api.StatusSuccess || len(good.Warnings) != 0 {
		t.Fatalf("an ordinary backup: %s %v", good.Status, good.Warnings)
	}
	if st := p.status(); st.Suspicious != 0 || st.Deleted != 1 {
		t.Fatalf("after ordinary backups: %+v", st)
	}

	encrypt(t, src, ".lockbit")
	bad := p.backup(pol.ID)
	if bad.Status != api.StatusWarning || !strings.Contains(strings.Join(bad.Warnings, "\n"), "Looks like ransomware") {
		t.Fatalf("the backup of encrypted files: %s %v", bad.Status, bad.Warnings)
	}
	var sus []api.Suspicion
	p.op.must("GET", "/api/v1/suspicions", nil, &sus)
	if len(sus) != 1 || sus[0].SnapshotID != bad.SnapshotID || sus[0].LastGood != good.SnapshotID || sus[0].Policy != "share" || sus[0].Agent != "fileserver" || sus[0].Reviewed != nil {
		t.Fatalf("suspicions: %+v", sus)
	}
	reasons := strings.Join(sus[0].Reasons, "\n")
	for _, want := range []string{"replaced by files of the same name with a new ending, most often “.lockbit”", "HOW_TO_DECRYPT_FILES.txt", "were changed or are gone"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons lack %q:\n%s", want, reasons)
		}
	}
	p.alerted("A backup of fileserver looks like ransomware", ".lockbit", good.SnapshotID)
	var h api.Health
	p.op.must("GET", "/api/v1/health", nil, &h)
	if h.State != "failed" || !strings.Contains(fmt.Sprint(h.Issues), "looks like ransomware") {
		t.Fatalf("health: %s %+v", h.State, h.Issues)
	}
	// The good snapshot stays although the rule keeps only one, also
	// after the next backup, and what was already deleted is not given
	// up when its time comes.
	p.backup(pol.ID)
	if live := p.live(); len(live) != 3 {
		t.Fatalf("live snapshots while held: %v", live)
	}
	p.srv.ExpireDeleted()
	time.Sleep(300 * time.Millisecond)
	if d := p.deleted(); len(d) != 1 || !d[0].Held {
		t.Fatalf("deleted snapshots while held: %+v", d)
	}

	// Someone looks and says it is fine: retention goes on.
	p.op.refused(403, "POST", "/api/v1/suspicions/"+bad.SnapshotID+"/review", nil)
	p.admin.must("POST", "/api/v1/suspicions/"+bad.SnapshotID+"/review", nil, nil)
	p.admin.refused(404, "POST", "/api/v1/suspicions/"+bad.SnapshotID+"/review", nil)
	p.op.must("GET", "/api/v1/suspicions", nil, &sus)
	if len(sus) != 1 || sus[0].ReviewedBy != "andrew" || sus[0].Reviewed == nil {
		t.Fatalf("after the review: %+v", sus)
	}
	waitFor(t, "the held snapshot to be given up", func() bool { return len(p.deleted()) == 0 })
	write(t, filepath.Join(src, "dept00", "after.txt"), []byte("back to work"))
	if j := p.backup(pol.ID); j.Status != api.StatusSuccess {
		t.Fatalf("a backup after the review: %s %v", j.Status, j.Warnings)
	}
	if live := p.live(); len(live) != 1 {
		t.Fatalf("live snapshots after the review: %v", live)
	}
}

// TestEncryptedInPlace: files that keep their names but turn into data
// that does not compress, and a burst of such data in a disk image.
func TestEncryptedInPlace(t *testing.T) {
	defer server.SetBurstBytes(1<<20, 256<<10)()
	p := newProtected(t, server.Config{})
	src := filepath.Join(t.TempDir(), "share")
	office(t, src, 4, 50)
	var pol api.Policy
	p.op.must("POST", "/api/v1/policies", api.Policy{Name: "share", AgentID: p.agentID, Paths: []string{src}, Enabled: true}, &pol)
	p.backup(pol.ID)
	// A third of the files, overwritten with random bytes.
	n := 0
	filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if n++; n%3 == 0 {
				write(t, path, rnd(int64(n), 8<<10))
			}
		}
		return nil
	})
	j := p.backup(pol.ID)
	if j.Status != api.StatusWarning || !strings.Contains(strings.Join(j.Warnings, "\n"), "does not compress") {
		t.Fatalf("files encrypted in place: %s %v", j.Status, j.Warnings)
	}

	// One big file, as a disk image is: small changes beside it, then a burst.
	img := filepath.Join(t.TempDir(), "vm")
	text := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 400000))
	write(t, filepath.Join(img, "disk.raw"), text)
	p.op.must("POST", "/api/v1/policies", api.Policy{Name: "image", AgentID: p.agentID, Paths: []string{img}, Enabled: true}, &pol)
	for i := 0; i < 4; i++ {
		write(t, filepath.Join(img, "changes.log"), []byte(fmt.Sprintf("a small change, number %d", i)))
		if j := p.backup(pol.ID); j.Status != api.StatusSuccess {
			t.Fatalf("image backup %d: %s %v", i, j.Status, j.Warnings)
		}
	}
	write(t, filepath.Join(img, "disk.raw"), rnd(5, len(text)))
	j = p.backup(pol.ID)
	if j.Status != api.StatusWarning || !strings.Contains(strings.Join(j.Warnings, "\n"), "times what a backup of this policy usually adds") {
		t.Fatalf("a burst of random data: %s %v", j.Status, j.Warnings)
	}
}
