package searchindex

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// store is a repository in memory.
type store map[keys.ID][]byte

func (s store) put(b []byte) (keys.ID, error) {
	id := keys.ID(sha256.Sum256(b))
	s[id] = append([]byte(nil), b...)
	return id, nil
}

func (s store) get(id keys.ID) ([]byte, error) {
	b, ok := s[id]
	if !ok {
		return nil, fmt.Errorf("no chunk %s", id.Short())
	}
	return b, nil
}

// file is one file of a made-up machine: its size, which stands for
// its version too.
type files map[string]int64

// snapshot stores the files as a snapshot with one source, "/data".
func (s store) snapshot(t *testing.T, id string, at time.Time, agent, policy string, fs files) *tree.Snapshot {
	t.Helper()
	var build func(dir string) tree.Entry
	build = func(dir string) tree.Entry {
		kids := map[string]bool{}
		for p := range fs {
			if strings.HasPrefix(p, dir+"/") {
				kids[strings.SplitN(p[len(dir)+1:], "/", 2)[0]] = true
			}
		}
		names := make([]string, 0, len(kids))
		for k := range kids {
			names = append(names, k)
		}
		sort.Strings(names)
		w := tree.NewPageWriter(s.put)
		for _, n := range names {
			p := dir + "/" + n
			if size, ok := fs[p]; ok {
				w.Add(&tree.Entry{Name: n, Type: tree.TypeFile, Size: size, ModTime: size * 1e9})
			} else {
				e := build(p)
				w.Add(&e)
			}
		}
		pages, err := w.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return tree.Entry{Name: filepath.Base(dir), Type: tree.TypeDir, Subtree: pages}
	}
	root := build("/data")
	root.Name = "/data"
	return &tree.Snapshot{ID: id, Time: at, AgentID: agent, PolicyID: policy, Sources: []tree.Entry{root}}
}

func contains(q string) func(string) bool {
	return func(n string) bool { return strings.Contains(strings.ToLower(n), q) }
}

func syncAll(t *testing.T, ix *Index, st store, snaps []*tree.Snapshot) {
	t.Helper()
	pending, err := ix.Reconcile(snaps)
	if err != nil {
		t.Fatal(err)
	}
	for _, sn := range pending {
		if err := ix.Add(context.Background(), st.get, sn); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndex(t *testing.T) {
	st := store{}
	ix, err := Open(filepath.Join(t.TempDir(), "search.db"), "repo")
	if err != nil {
		t.Fatal(err)
	}
	// ix is opened a second time further down: close that one.
	defer func() { ix.Close() }()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s1 := st.snapshot(t, "s1", t0, "a", "p", files{"/data/Reports/Budget 2026.xlsx": 10, "/data/Reports/old/budget-2019.xls": 5, "/data/notes.txt": 1})
	s2 := st.snapshot(t, "s2", t0.Add(time.Hour), "a", "p", files{"/data/Reports/Budget 2026.xlsx": 12, "/data/Reports/Budget 2027.xlsx": 3, "/data/notes.txt": 1})
	s3 := st.snapshot(t, "s3", t0.Add(2*time.Hour), "a", "p", files{"/data/Reports/Budget 2026.xlsx": 12, "/data/Reports/old/budget-2019.xls": 5, "/data/notes.txt": 1})
	syncAll(t, ix, st, []*tree.Snapshot{s1, s2, s3})

	find := func(q string) map[string]Hit {
		t.Helper()
		r, err := ix.Search(context.Background(), Query{Text: q, Match: contains(q)})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Hit{}
		for _, h := range r.Hits {
			out[strings.Join(h.Path, "/")] = h
		}
		return out
	}
	got := find("budget")
	if len(got) != 3 {
		t.Fatalf("hits: %+v", got)
	}
	if h := got["/data/Reports/Budget 2026.xlsx"]; h.Snapshots != 3 || h.Newest != "s3" || h.Oldest != "s1" || h.Size != 12 || h.Type != tree.TypeFile {
		t.Fatalf("a file in every snapshot: %+v", h)
	}
	// Gone in the second snapshot and back in the third.
	if h := got["/data/Reports/old/budget-2019.xls"]; h.Snapshots != 2 || h.Newest != "s3" || h.Oldest != "s1" {
		t.Fatalf("a file that came back: %+v", h)
	}
	if h := got["/data/Reports/Budget 2027.xlsx"]; h.Snapshots != 1 || h.Newest != "s2" {
		t.Fatalf("a file of one snapshot: %+v", h)
	}
	if h := find("old")["/data/Reports/old"]; h.Type != tree.TypeDir || h.Snapshots != 2 {
		t.Fatalf("a folder: %+v", h)
	}
	// Two letters are too few for the index of names; every name is read.
	if got := find("ol"); len(got) != 1 {
		t.Fatalf("two letters: %+v", got)
	}

	// Forget the middle snapshot: what only it held is gone.
	syncAll(t, ix, st, []*tree.Snapshot{s1, s3})
	got = find("budget")
	if len(got) != 2 || got["/data/Reports/Budget 2026.xlsx"].Snapshots != 2 {
		t.Fatalf("after forgetting: %+v", got)
	}
	if st := ix.Status(); st.Snapshots != 2 || st.Names != 6 || st.Bytes == 0 {
		t.Fatalf("status: %+v", st)
	}
	var n int
	ix.r.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n)
	if n != 6 {
		t.Fatalf("%d names are left, want 6", n)
	}
	// The repository is put back from a copy made before that, and the
	// forgotten snapshot is there again.
	syncAll(t, ix, st, []*tree.Snapshot{s1, s2, s3})
	if got = find("budget"); len(got) != 3 || got["/data/Reports/Budget 2026.xlsx"].Snapshots != 3 || got["/data/Reports/Budget 2027.xlsx"].Newest != "s2" {
		t.Fatalf("after a forgotten snapshot came back: %+v", got)
	}
	// A snapshot older than the ones in the index turns up.
	s0 := st.snapshot(t, "s0", t0.Add(-time.Hour), "a", "p", files{"/data/Reports/Budget 2025.xlsx": 7, "/data/notes.txt": 1})
	syncAll(t, ix, st, []*tree.Snapshot{s1, s2, s3, s0})
	if got = find("budget"); len(got) != 4 || got["/data/Reports/Budget 2025.xlsx"].Newest != "s0" || got["/data/Reports/Budget 2026.xlsx"].Oldest != "s1" {
		t.Fatalf("after an older snapshot was added: %+v", got)
	}
	if h := find("notes")["/data/notes.txt"]; h.Snapshots != 4 || h.Oldest != "s0" || h.Newest != "s3" {
		t.Fatalf("a file in all four: %+v", h)
	}
	// Capital letters and letters with accents are found either way.
	s4 := st.snapshot(t, "s4", t0.Add(3*time.Hour), "a", "p", files{"/data/Ärger über Größe.TXT": 2})
	syncAll(t, ix, st, []*tree.Snapshot{s1, s2, s3, s0, s4})
	for _, q := range []string{"ärger", "ÜBER", "größe.txt"} {
		r, err := ix.Search(context.Background(), Query{Text: q, Match: func(n string) bool { return strings.Contains(strings.ToLower(n), strings.ToLower(q)) }})
		if err != nil || len(r.Hits) != 1 {
			t.Fatalf("search for %q: %+v, %v", q, r, err)
		}
	}
	// Open it again as the index of another repository: it is emptied.
	ix.Close()
	if ix, err = Open(ix.path, "other"); err != nil {
		t.Fatal(err)
	}
	if st := ix.Status(); st.Snapshots != 0 || st.Names != 0 {
		t.Fatalf("status of an emptied index: %+v", st)
	}
}

// TestAgainstWalk makes machines whose files come, change and go at
// random, forgets snapshots at random, and compares every answer of the
// index with one worked out from the snapshots themselves.
func TestAgainstWalk(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	st := store{}
	path := filepath.Join(t.TempDir(), "search.db")
	ix, err := Open(path, "repo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ix.Close() }()
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	name := func() string {
		p := "/data"
		for d := rng.Intn(4); d > 0; d-- {
			p += "/" + words[rng.Intn(3)]
		}
		return p + "/" + words[rng.Intn(len(words))] + fmt.Sprint(rng.Intn(6)) + ".txt"
	}
	type series struct {
		agent, policy string
		fs            files
	}
	lines := []*series{{"a", "p1", files{}}, {"a", "p2", files{}}, {"b", "p1", files{}}}
	type kept struct {
		sn *tree.Snapshot
		fs files
	}
	var snaps []kept
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	check := func(step int) {
		t.Helper()
		for _, agent := range []string{"", "a", "b"} {
			for _, q := range []string{"alpha", "tro", "o", "3.txt", "bravo"} {
				// The answer from the snapshots: every path, file or
				// folder, with the snapshots that hold it.
				type where struct{ ids []string }
				want := map[string]*where{}
				for _, k := range snaps {
					if agent != "" && k.sn.AgentID != agent {
						continue
					}
					seen := map[string]bool{}
					for p := range k.fs {
						for ; p != "/data"; p = p[:strings.LastIndex(p, "/")] {
							if seen[p] {
								break
							}
							seen[p] = true
							if !strings.Contains(p[strings.LastIndex(p, "/")+1:], q) {
								continue
							}
							key := k.sn.AgentID + " " + k.sn.PolicyID + " " + p
							if want[key] == nil {
								want[key] = &where{}
							}
							want[key].ids = append(want[key].ids, k.sn.ID)
						}
					}
				}
				r, err := ix.Search(context.Background(), Query{Text: q, Match: contains(q), Agent: agent, Limit: 100000})
				if err != nil {
					t.Fatal(err)
				}
				if len(r.Hits) != len(want) {
					t.Fatalf("step %d, %q on %q: %d hits, want %d", step, q, agent, len(r.Hits), len(want))
				}
				byID := map[string]*tree.Snapshot{}
				for _, k := range snaps {
					byID[k.sn.ID] = k.sn
				}
				for _, h := range r.Hits {
					sn := byID[h.Newest]
					w := want[sn.AgentID+" "+sn.PolicyID+" "+strings.Join(h.Path, "/")]
					if w == nil || h.Snapshots != len(w.ids) || h.Oldest != w.ids[0] || h.Newest != w.ids[len(w.ids)-1] {
						t.Fatalf("step %d, %q: hit %+v, want %+v", step, q, h, w)
					}
				}
			}
		}
	}
	for step := 0; step < 120; step++ {
		l := lines[rng.Intn(len(lines))]
		for n := rng.Intn(6); n > 0; n-- {
			l.fs[name()] = int64(step + 1)
		}
		for p := range l.fs {
			if rng.Intn(12) == 0 {
				delete(l.fs, p)
			}
		}
		if rng.Intn(15) == 0 {
			// A whole folder goes.
			for p := range l.fs {
				if strings.HasPrefix(p, "/data/"+words[rng.Intn(3)]+"/") {
					delete(l.fs, p)
				}
			}
		}
		fs := files{}
		for p, v := range l.fs {
			fs[p] = v
		}
		at = at.Add(time.Hour)
		snaps = append(snaps, kept{st.snapshot(t, fmt.Sprintf("s%03d", step), at, l.agent, l.policy, fs), fs})
		// Forget some: often the oldest, sometimes any, the newest too.
		for rng.Intn(3) == 0 && len(snaps) > 1 {
			i := 0
			if rng.Intn(2) == 0 {
				i = rng.Intn(len(snaps))
			}
			snaps = append(snaps[:i], snaps[i+1:]...)
		}
		var all []*tree.Snapshot
		for _, k := range snaps {
			all = append(all, k.sn)
		}
		pending, err := ix.Reconcile(all)
		if err != nil {
			t.Fatal(err)
		}
		for _, sn := range pending {
			if step%7 == 3 {
				// Stopped part of the way, as when the server is shut
				// down, and picked up again.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				reads := 0
				ix.Add(ctx, func(id keys.ID) ([]byte, error) {
					if reads++; reads > 2 {
						return nil, context.Canceled
					}
					return st.get(id)
				}, sn)
			}
			if err := ix.Add(context.Background(), st.get, sn); err != nil {
				t.Fatal(err)
			}
		}
		check(step)
		if step%40 == 20 {
			ix.Close()
			if ix, err = Open(path, "repo"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Nothing is left behind once every snapshot is forgotten.
	if _, err := ix.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	var nodes, past, names int
	ix.r.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes)
	ix.r.QueryRow(`SELECT COUNT(*) FROM past`).Scan(&past)
	if nodes != 0 || past != 0 || ix.Status().Names != 0 {
		t.Fatalf("%d names and %d earlier times are left (%d counted)", nodes, past, ix.Status().Names)
	}
	_ = names
}
