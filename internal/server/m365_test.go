package server_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/m365/m365test"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestM365 backs up mail and OneDrive of a stand-in for Microsoft 365
// through the server, an agent and the admin API, and restores them
// into a folder. See package m365test for the stand-in. Nothing here
// has met a real tenant.
func TestM365(t *testing.T) {
	ms := m365test.New()
	defer ms.Close()
	e, agentID := stack(t)
	tenant := func(secret string) api.M365Tenant {
		return api.M365Tenant{Name: "Example", TenantID: ms.TenantID, ClientID: ms.ClientID, ClientSecret: secret, LoginURL: ms.URL, GraphURL: ms.URL}
	}
	refused := func(want string, method, path string, in any) {
		t.Helper()
		if err := e.admin.Do(method, path, in, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s %s: got %v, want an error about %q", method, path, err, want)
		}
	}

	// A tenant is checked before it is saved, and tried as typed.
	refused("client secret", "POST", "/api/v1/m365-tenants", tenant(""))
	bad := tenant(ms.Secret)
	bad.GraphURL = ""
	refused("both addresses", "POST", "/api/v1/m365-tenants", bad)
	refused("client secret is wrong", "POST", "/api/v1/m365-tenants/test", tenant("nope"))
	bad = tenant(ms.Secret)
	bad.ClientID = "someone-else"
	refused("not one of this tenant", "POST", "/api/v1/m365-tenants/test", bad)
	bad = tenant(ms.Secret)
	bad.TenantID = "who"
	refused("tenant) id is wrong", "POST", "/api/v1/m365-tenants/test", bad)
	ms.Lock()
	ms.Deny["users"] = true
	ms.Unlock()
	refused("User.Read.All", "POST", "/api/v1/m365-tenants/test", tenant(ms.Secret))
	ms.Lock()
	ms.Deny["users"] = false
	ms.Unlock()
	var count map[string]int
	e.do("POST", "/api/v1/m365-tenants/test", tenant(ms.Secret), &count)
	if count["users"] != 4 || count["licensed"] != 2 {
		t.Fatalf("users of the tenant: %v", count)
	}
	var tn api.M365Tenant
	e.do("POST", "/api/v1/m365-tenants", tenant(ms.Secret), &tn)
	if tn.ID == "" || tn.ClientSecret != "" || !tn.SecretSet {
		t.Fatalf("saved tenant: %+v", tn)
	}
	// Editing keeps the secret, and the stored one can be tried.
	keep := tenant("")
	keep.SecretSet, keep.Name = true, "Example Ltd"
	e.do("PUT", "/api/v1/m365-tenants/"+tn.ID, keep, &tn)
	keep.ID = tn.ID
	e.do("POST", "/api/v1/m365-tenants/test", keep, &count)
	var users []api.M365User
	e.do("GET", "/api/v1/m365-tenants/"+tn.ID+"/users", nil, &users)
	if len(users) != 4 || users[0].UPN != "Alice@example.com" || !users[0].Licensed {
		t.Fatalf("users: %+v", users)
	}

	// A policy names a tenant, users and what to take.
	pol := func(users ...string) api.Policy {
		return api.Policy{Name: "m365", AgentID: agentID, M365: &api.M365Spec{Tenant: tn.ID, Users: users, Mail: true, OneDrive: true}}
	}
	p := pol("*")
	p.M365.Tenant = "gone"
	refused("choose the Microsoft 365 tenant", "POST", "/api/v1/policies", p)
	refused("name the Microsoft 365 users", "POST", "/api/v1/policies", pol())
	refused("sign-in name", "POST", "/api/v1/policies", pol("alice"))
	p = pol("*")
	p.M365.Mail, p.M365.OneDrive = false, false
	refused("mail, OneDrive or both", "POST", "/api/v1/policies", p)
	e.do("POST", "/api/v1/policies", pol("*", " * "), &p)
	if len(p.M365.Users) != 1 {
		t.Fatalf("users of the saved policy: %q", p.M365.Users)
	}

	// The first backup takes everything of the two licensed users. The
	// room has no licence and the guest is a guest; Bob has no OneDrive.
	j, st := e.backup(p.ID)
	if j.Status != api.StatusSuccess || len(j.Warnings) != 0 {
		t.Fatalf("backup: %s %q %v", j.Status, j.Error, j.Warnings)
	}
	if st.Files != 9 || st.FilesChanged != 9 {
		t.Fatalf("first backup: %+v", st)
	}
	out := t.TempDir()
	var rj api.Job
	refused("restored into a folder", "POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j.SnapshotID})
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j.SnapshotID, Target: out}, &rj)
	if done := e.waitLong(rj.ID); done.Status != api.StatusSuccess {
		t.Fatalf("restore: %q %v", done.Error, done.Warnings)
	}
	got := listFiles(t, out)
	want := []string{
		"Microsoft 365/alice@example.com/Mail/Drafts/2026-11-20 0930 Unfinished [*].eml",
		"Microsoft 365/alice@example.com/Mail/Inbox/2026-11-20 0930 Quarterly numbers [*].eml",
		"Microsoft 365/alice@example.com/Mail/Inbox/2026-11-20 1030 Re_ plans _ _next_ year_ [*].eml",
		"Microsoft 365/alice@example.com/Mail/Inbox/2026-11-20 1130 (no subject) [*].eml",
		"Microsoft 365/alice@example.com/Mail/Inbox/Projects/2026-11-20 1230 Kickoff [*].eml",
		"Microsoft 365/alice@example.com/OneDrive/Budget.xlsx",
		"Microsoft 365/alice@example.com/OneDrive/Notes.txt",
		"Microsoft 365/alice@example.com/OneDrive/Reports/2026.docx",
		"Microsoft 365/bob@example.com/Mail/Inbox/2026-11-20 0930 Hello Bob [*].eml",
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("restored files:\n%s", strings.Join(got, "\n"))
	}
	for i := range want {
		if !sameBut(want[i], got[i]) {
			t.Fatalf("restored file %d: %q, want %q", i, got[i], want[i])
		}
	}
	read := func(dir, rel string) string {
		t.Helper()
		m, _ := filepath.Glob(filepath.Join(dir, "Microsoft 365", filepath.FromSlash(rel)))
		if len(m) != 1 {
			t.Fatalf("%s: %d files", rel, len(m))
		}
		b, err := os.ReadFile(m[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := read(out, "alice@example.com/Mail/Inbox/Projects/*Kickoff*.eml"); !strings.Contains(s, "Subject: Kickoff") || !strings.Contains(s, "We start on the first.") {
		t.Fatalf("restored message: %q", s)
	}
	if s := read(out, "alice@example.com/OneDrive/Reports/2026.docx"); s != strings.Repeat("report ", 2000) {
		t.Fatalf("restored file holds %d bytes", len(s))
	}
	if fi, err := os.Stat(filepath.Join(out, "Microsoft 365", "alice@example.com", "OneDrive", "Notes.txt")); err != nil || !fi.ModTime().Equal(time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("restored file's time: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(out, "Microsoft 365", "alice@example.com", "OneDrive", "Reports", "Old")); err != nil {
		t.Fatalf("the empty folder was not restored: %v", err)
	}

	// Nothing changed: the second backup downloads nothing.
	downloads := func() int {
		ms.Lock()
		defer ms.Unlock()
		n := 0
		for _, c := range ms.Downloads {
			n += c
		}
		return n
	}
	before := downloads()
	j, st = e.backup(p.ID)
	if j.Error != "" || len(j.Warnings) != 0 || st.Files != 9 || st.FilesChanged != 0 || downloads() != before {
		t.Fatalf("second backup: %q %v %+v, %d downloads", j.Error, j.Warnings, st, downloads()-before)
	}

	// A new message, a changed file, an edited draft and a deleted file:
	// only those three are downloaded. A message and a file that cannot
	// be read stay as they were, with a warning each.
	ms.Lock()
	alice := ms.Users[0]
	inbox := alice.Mail[0]
	inbox.Messages = append(inbox.Messages, &m365test.Message{ID: "m10", Subject: "Fresh", Received: time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC),
		MIME: "Subject: Fresh\r\n\r\nNew today.\r\n"})
	alice.Mail[1].Messages[0].Change, alice.Mail[1].Messages[0].MIME = "c2", "Subject: Unfinished\r\n\r\nFirst words, and more\r\n"
	notes := alice.Drive.Children[1]
	notes.Data, notes.CTag = "remember the bread", "c:{i2},2"
	budget := alice.Drive.Children[0]
	budget.Data, budget.CTag = "lost", "c:{i1},2"
	ms.Broken["i1"] = true
	alice.Drive.Children[2].Children = alice.Drive.Children[2].Children[1:]
	ms.Unlock()
	before = downloads()
	j, st = e.backup(p.ID)
	if j.Status != api.StatusWarning || len(j.Warnings) != 1 || !strings.Contains(j.Warnings[0], "Budget.xlsx") || !strings.Contains(j.Warnings[0], "kept as it was") {
		t.Fatalf("third backup: %s %q %v", j.Status, j.Error, j.Warnings)
	}
	if st.Files != 9 || st.FilesChanged != 3 || downloads()-before != 4 {
		t.Fatalf("third backup: %+v, %d downloads", st, downloads()-before)
	}
	out = t.TempDir()
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j.SnapshotID, Target: out, Include: []string{"Microsoft 365/alice@example.com"}}, &rj)
	if done := e.waitLong(rj.ID); done.Status != api.StatusSuccess {
		t.Fatalf("restore: %q %v", done.Error, done.Warnings)
	}
	if s := read(out, "alice@example.com/OneDrive/Notes.txt"); s != "remember the bread" {
		t.Fatalf("changed file: %q", s)
	}
	if s := read(out, "alice@example.com/OneDrive/Budget.xlsx"); s != strings.Repeat("budget ", 500) {
		t.Fatalf("the file that could not be read holds %d bytes", len(s))
	}
	if s := read(out, "alice@example.com/Mail/Drafts/*.eml"); !strings.Contains(s, "and more") {
		t.Fatalf("edited draft: %q", s)
	}
	if s := read(out, "alice@example.com/Mail/Inbox/*Fresh*.eml"); !strings.Contains(s, "New today.") {
		t.Fatalf("new message: %q", s)
	}
	if files := listFiles(t, out); len(files) != 8 {
		t.Fatalf("restored files of Alice:\n%s", strings.Join(files, "\n"))
	}

	// A missing permission fails the part it covers, and says which.
	ms.Lock()
	ms.Deny["mail"] = true
	ms.Broken = map[string]bool{}
	ms.Unlock()
	j, _ = e.backup(p.ID)
	if j.Status != api.StatusWarning || len(j.Warnings) != 2 || !strings.Contains(j.Warnings[0], "Mail.Read") || !strings.Contains(j.Warnings[0], "kept as it was") {
		t.Fatalf("backup without Mail.Read: %s %q %v", j.Status, j.Error, j.Warnings)
	}
	ms.Lock()
	ms.Deny["mail"] = false
	ms.Unlock()

	// Named users: one without a mailbox and one who does not exist are
	// said so; a wrong secret fails the job with the reason.
	var named api.Policy
	np := pol("room1@example.com", "nobody@example.com", "BOB@example.com")
	np.Name = "named"
	e.do("POST", "/api/v1/policies", np, &named)
	j, st = e.backup(named.ID)
	all := strings.Join(j.Warnings, "\n")
	if j.Status != api.StatusWarning || st.Files != 1 || !strings.Contains(all, "no user nobody@example.com") || !strings.Contains(all, "room1@example.com has no mailbox") || !strings.Contains(all, "bob@example.com has no OneDrive") {
		t.Fatalf("backup of named users: %s %q %v %+v", j.Status, j.Error, j.Warnings, st)
	}
	e.do("PUT", "/api/v1/m365-tenants/"+tn.ID, tenant("wr0ng~secret"), &tn)
	var bj api.Job
	e.do("POST", "/api/v1/policies/"+named.ID+"/run", nil, &bj)
	if done := e.waitLong(bj.ID); done.Status != api.StatusFailed || !strings.Contains(done.Error, "client secret is wrong or has expired") {
		t.Fatalf("backup with a wrong secret: %s %q", done.Status, done.Error)
	}

	// A tenant that policies use stays, and no list shows the secret.
	refused("policy", "DELETE", "/api/v1/m365-tenants/"+tn.ID, nil)
	e.do("PUT", "/api/v1/m365-tenants/"+tn.ID, tenant(ms.Secret), &tn)
	for _, path := range []string{"/api/v1/m365-tenants", "/api/v1/jobs?limit=100", "/api/v1/policies", "/api/v1/audit"} {
		var v json.RawMessage
		e.do("GET", path, nil, &v)
		if bytes.Contains(v, []byte(ms.Secret)) || bytes.Contains(v, []byte("wr0ng~secret")) {
			t.Fatalf("%s shows the client secret", path)
		}
	}
	var sn tree.Snapshot
	e.do("GET", "/api/v1/snapshots/"+j.SnapshotID, nil, &sn)
	if len(sn.Sources) != 1 || sn.Sources[0].Name != tree.M365Name {
		t.Fatalf("sources of the snapshot: %+v", sn.Sources)
	}
	e.do("DELETE", "/api/v1/policies/"+p.ID, nil, nil)
	e.do("DELETE", "/api/v1/policies/"+named.ID, nil, nil)
	e.do("DELETE", "/api/v1/m365-tenants/"+tn.ID, nil, nil)
}

// sameBut compares a path with a pattern in which [*] stands for the
// letters that come from a message's id.
func sameBut(pattern, path string) bool {
	head, tail, ok := strings.Cut(pattern, "[*]")
	if !ok {
		return pattern == path
	}
	return strings.HasPrefix(path, head) && strings.HasSuffix(path, tail) && len(path) == len(head)+len(tail)+10
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
