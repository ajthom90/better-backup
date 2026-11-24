package server_test

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// TestDownloadAndSearch downloads a file and a folder of a snapshot the
// way a browser does, and searches for files by name across snapshots.
func TestDownloadAndSearch(t *testing.T) {
	e, agentID := stack(t)
	src := filepath.Join(t.TempDir(), "data")
	put := func(rel string, data []byte) {
		t.Helper()
		p := filepath.Join(src, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, 6<<20)
	rand.New(rand.NewSource(4)).Read(big)
	put("Reports/Budget 2026.xlsx", []byte(strings.Repeat("numbers ", 400)))
	put("Reports/old/budget-2019.xls", []byte("old numbers"))
	put("Reports/video.bin", big)
	put("notes.txt", []byte("remember"))
	put("empty/.keep", nil)
	var p api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "docs", AgentID: agentID, Paths: []string{src}}, &p)
	j1, _ := e.backup(p.ID)
	if j1.Status != api.StatusSuccess {
		t.Fatalf("backup: %s %q", j1.Status, j1.Error)
	}

	browser := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	fetch := func(url string) (int, http.Header, []byte) {
		t.Helper()
		resp, err := browser.Get(e.admin.Base() + url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, b
	}
	link := func(snapshot, path string) api.DownloadLink {
		t.Helper()
		var l api.DownloadLink
		e.do("POST", "/api/v1/snapshots/"+snapshot+"/download", api.DownloadRequest{Path: path}, &l)
		return l
	}

	// One file: its bytes, its name, its size known in advance.
	l := link(j1.SnapshotID, filepath.Join(src, "Reports", "video.bin"))
	if l.Name != "video.bin" || l.Size != int64(len(big)) {
		t.Fatalf("link: %+v", l)
	}
	code, h, body := fetch(l.URL)
	if code != 200 || !bytes.Equal(body, big) || !strings.Contains(h.Get("Content-Disposition"), "video.bin") || h.Get("Content-Length") != "6291456" {
		t.Fatalf("download: %d, %d bytes, %v", code, len(body), h)
	}
	// A link works once, and only a signed-in operator gets one.
	if code, _, _ := fetch(l.URL); code != http.StatusGone {
		t.Fatalf("a link used twice: %d", code)
	}
	if code, _, _ := fetch("/api/v1/downloads/nonsense"); code != http.StatusGone {
		t.Fatalf("a made-up link: %d", code)
	}
	resp, err := browser.Post(e.admin.Base()+"/api/v1/snapshots/"+j1.SnapshotID+"/download", "application/json", strings.NewReader(`{"path":"x"}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a link without signing in: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := e.admin.Do("POST", "/api/v1/snapshots/"+j1.SnapshotID+"/download", api.DownloadRequest{Path: filepath.Join(src, "nothing")}, nil); err == nil || !strings.Contains(err.Error(), "not in this snapshot") {
		t.Fatalf("a link to nothing: %v", err)
	}

	// A folder: a zip file with everything under it.
	l = link(j1.SnapshotID, filepath.Join(src, "Reports"))
	if l.Name != "Reports.zip" {
		t.Fatalf("link: %+v", l)
	}
	code, h, body = fetch(l.URL)
	if code != 200 || h.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip download: %d %v", code, h)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		got[f.Name] = string(b)
	}
	if len(got) != 5 || got["Reports/Budget 2026.xlsx"] != strings.Repeat("numbers ", 400) || got["Reports/old/budget-2019.xls"] != "old numbers" || got["Reports/video.bin"] != string(big) {
		names := []string{}
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("zip holds %q", names)
	}
	// The whole source, whose name is a path.
	l = link(j1.SnapshotID, src)
	if l.Name != "data.zip" {
		t.Fatalf("link to a source: %+v", l)
	}
	if code, _, body = fetch(l.URL); code != 200 {
		t.Fatalf("zip of the source: %d", code)
	}
	if zr, err = zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil || len(zr.File) != 9 {
		t.Fatalf("zip of the source: %v, %d entries", err, len(zr.File))
	}

	// A second snapshot: one file gone, one new.
	os.Remove(filepath.Join(src, "Reports", "old", "budget-2019.xls"))
	put("Reports/Budget 2027.xlsx", []byte("next year"))
	j2, _ := e.backup(p.ID)

	search := func(q string) api.SearchResult {
		t.Helper()
		var r api.SearchResult
		e.do("GET", "/api/v1/search?q="+strings.ReplaceAll(q, " ", "%20"), nil, &r)
		return r
	}
	r := search("BUDGET")
	if !r.Indexed || r.Behind != 0 {
		t.Fatalf("the search was not answered from the index: %+v", r)
	}
	if r.Snapshots != 2 || len(r.Hits) != 3 || r.More {
		t.Fatalf("search: %+v", r)
	}
	by := map[string]api.SearchHit{}
	for _, h := range r.Hits {
		by[h.Name] = h
	}
	if h := by["Budget 2026.xlsx"]; h.Snapshots != 2 || h.SnapshotID != j2.SnapshotID || h.Size != 3200 || h.Path != filepath.Join(src, "Reports", "Budget 2026.xlsx") || !h.Oldest.Before(h.Time) {
		t.Fatalf("a file in both snapshots: %+v", h)
	}
	if h := by["Budget 2027.xlsx"]; h.Snapshots != 1 || h.SnapshotID != j2.SnapshotID {
		t.Fatalf("a new file: %+v", h)
	}
	// A deleted file is found in the snapshot that still has it, and
	// can be downloaded from there.
	h1 := by["budget-2019.xls"]
	if h1.Snapshots != 1 || h1.SnapshotID != j1.SnapshotID {
		t.Fatalf("a deleted file: %+v", h1)
	}
	if code, _, body = fetch(link(h1.SnapshotID, h1.Path).URL); code != 200 || string(body) != "old numbers" {
		t.Fatalf("download of a deleted file: %d %q", code, body)
	}
	if r = search("*.xls"); len(r.Hits) != 1 || r.Hits[0].Name != "budget-2019.xls" {
		t.Fatalf("pattern search: %+v", r.Hits)
	}
	if r = search("reports"); len(r.Hits) != 1 || r.Hits[0].Type != "dir" {
		t.Fatalf("folder search: %+v", r.Hits)
	}
	if r = search("no such thing"); len(r.Hits) != 0 || r.Snapshots != 2 {
		t.Fatalf("empty search: %+v", r)
	}
	if err := e.admin.Do("GET", "/api/v1/search?q=a", nil, nil); err == nil || !strings.Contains(err.Error(), "two letters") {
		t.Fatalf("a one-letter search: %v", err)
	}
	if err := e.admin.Do("GET", "/api/v1/search?q=%5Bab", nil, nil); err != nil {
		t.Fatalf("a search with a bracket: %v", err)
	}

	// The index of file names can be built again from the snapshots.
	// Searches go on while it is, by reading the snapshots, and find
	// the same.
	var ixs api.SearchIndexStatus
	e.do("GET", "/api/v1/search/index", nil, &ixs)
	if ixs.State != "ready" || ixs.Snapshots != 2 || ixs.Waiting != 0 || ixs.Names < 8 || ixs.Bytes == 0 {
		t.Fatalf("index status: %+v", ixs)
	}
	names := ixs.Names
	e.do("POST", "/api/v1/search/index/rebuild", nil, &ixs)
	if ixs.State != "building" {
		t.Fatalf("index status after asking for it again: %+v", ixs)
	}
	if r = search("budget"); len(r.Hits) != 3 || r.Snapshots != 2 {
		t.Fatalf("search while the index is built: %+v", r)
	}
	for start := time.Now(); ixs.State != "ready"; time.Sleep(20 * time.Millisecond) {
		if time.Since(start) > 30*time.Second {
			t.Fatalf("the index was not built again: %+v", ixs)
		}
		e.do("GET", "/api/v1/search/index", nil, &ixs)
	}
	if r = search("budget"); !r.Indexed || len(r.Hits) != 3 || ixs.Names != names {
		t.Fatalf("search after the index was built again: %+v, %+v", r, ixs)
	}
	// A forgotten snapshot takes the files only it held out of the index.
	e.do("DELETE", "/api/v1/snapshots/"+j1.SnapshotID, nil, nil)
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		e.do("GET", "/api/v1/search/index", nil, &ixs)
		if ixs.Names < names {
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("the forgotten snapshot's files stayed in the index: %+v", ixs)
		}
	}
	if r = search("budget"); !r.Indexed || len(r.Hits) != 2 || r.Snapshots != 1 || r.Hits[0].Snapshots != 1 {
		t.Fatalf("search after a snapshot was forgotten: %+v", r)
	}

	// Downloads are in the activity log.
	var audit []api.AuditEntry
	e.do("GET", "/api/v1/audit", nil, &audit)
	n := 0
	for _, a := range audit {
		if a.Action == "Downloaded from a snapshot" && strings.Contains(a.Detail, "video.bin") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d activity log entries for the download", n)
	}
}
