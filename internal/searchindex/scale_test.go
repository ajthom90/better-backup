package searchindex

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/tree"
)

type scaleDir struct {
	name  string
	up    *scaleDir
	dirs  []*scaleDir
	files []tree.Entry
	dirty bool
	entry tree.Entry
}

func (d *scaleDir) touch() {
	for ; d != nil && !d.dirty; d = d.up {
		d.dirty = true
	}
}

func (d *scaleDir) build(st store) tree.Entry {
	if !d.dirty {
		return d.entry
	}
	w := tree.NewPageWriter(st.put)
	for _, k := range d.dirs {
		e := k.build(st)
		w.Add(&e)
	}
	for i := range d.files {
		w.Add(&d.files[i])
	}
	pages, _ := w.Finish()
	d.entry, d.dirty = tree.Entry{Name: d.name, Type: tree.TypeDir, Subtree: pages}, false
	return d.entry
}

// TestScale indexes a made-up file server of BB_INDEX_SCALE files and a
// month of daily backups of it, and says how long things take and how
// big the index is. It is not run unless the variable is set.
func TestScale(t *testing.T) {
	want, _ := strconv.Atoi(os.Getenv("BB_INDEX_SCALE"))
	if want == 0 {
		t.Skip("set BB_INDEX_SCALE to the number of files")
	}
	rng := rand.New(rand.NewSource(1))
	words := make([]string, 3000)
	for i := range words {
		b := make([]byte, 4+rng.Intn(6))
		for j := range b {
			b[j] = "aaabcdeeefghiiijklmnooopqrstuuvwxyz"[rng.Intn(35)]
		}
		words[i] = string(b)
	}
	exts := []string{".docx", ".xlsx", ".pdf", ".jpg", ".txt", ".msg", ".dwg", ".zip"}
	serial := 0
	file := func() tree.Entry {
		serial++
		return tree.Entry{Name: fmt.Sprintf("%s %s %d%s", words[rng.Intn(len(words))], words[rng.Intn(len(words))], rng.Intn(3000), exts[rng.Intn(len(exts))]),
			Type: tree.TypeFile, Size: int64(rng.Intn(1 << 22)), ModTime: int64(serial) * 1e9}
	}
	st := store{}
	root := &scaleDir{name: `D:\Shares`, dirty: true}
	var leaves []*scaleDir
	for n := 0; n < want; {
		a := &scaleDir{name: words[rng.Intn(len(words))] + fmt.Sprint(len(root.dirs)), up: root, dirty: true}
		root.dirs = append(root.dirs, a)
		for i := 0; i < 50 && n < want; i++ {
			b := &scaleDir{name: fmt.Sprintf("%s %d", words[rng.Intn(len(words))], i), up: a, dirty: true}
			a.dirs = append(a.dirs, b)
			for j := 0; j < 40 && n < want; j++ {
				c := &scaleDir{name: fmt.Sprintf("%s-%d", words[rng.Intn(len(words))], j), up: b, dirty: true}
				b.dirs = append(b.dirs, c)
				leaves = append(leaves, c)
				for k := 0; k < 50; k++ {
					c.files = append(c.files, file())
				}
				n += 50
			}
		}
	}
	path := filepath.Join(t.TempDir(), "search.db")
	if dir := os.Getenv("BB_INDEX_DIR"); dir != "" {
		path = filepath.Join(dir, "search.db")
		os.Remove(path)
	}
	ix, err := Open(path, "repo")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var snaps []*tree.Snapshot
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func() time.Duration {
		e := root.build(st)
		at = at.Add(24 * time.Hour)
		sn := &tree.Snapshot{ID: fmt.Sprintf("s%03d", len(snaps)), Time: at, AgentID: "a", PolicyID: "p", OS: "windows", Sources: []tree.Entry{e}}
		snaps = append(snaps, sn)
		start := time.Now()
		if err := ix.Add(context.Background(), st.get, sn); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	d := add()
	s := ix.Status()
	t.Logf("first backup: %d names indexed in %v (%.0f a second); index is %d MB, %.0f bytes a name",
		s.Names, d.Round(time.Second), float64(s.Names)/d.Seconds(), s.Bytes>>20, float64(s.Bytes)/float64(s.Names))
	// A month of days: each day 0.5% of the files change, 0.1% are new
	// and 0.1% are deleted.
	var total time.Duration
	const days = 30
	for day := 0; day < days; day++ {
		for i := 0; i < want/200; i++ {
			l := leaves[rng.Intn(len(leaves))]
			f := &l.files[rng.Intn(len(l.files))]
			f.Size++
			serial++
			f.ModTime = int64(serial) * 1e9
			l.touch()
		}
		for i := 0; i < want/1000; i++ {
			l := leaves[rng.Intn(len(leaves))]
			l.files = append(l.files, file())
			l.touch()
			l = leaves[rng.Intn(len(leaves))]
			if len(l.files) > 1 {
				l.files = l.files[1:]
				l.touch()
			}
		}
		total += add()
	}
	s2 := ix.Status()
	t.Logf("%d daily backups after it: %v each on average; index is %d MB with %d names", days, (total / days).Round(time.Millisecond), s2.Bytes>>20, s2.Names)
	for _, q := range []string{words[5] + " " + words[9][:2], words[7], ".xlsx", "*" + words[3][:3] + "*.pdf", words[11][:3], "zq", "no such file anywhere"} {
		lq := strings.ToLower(q)
		m := contains(lq)
		if strings.Contains(q, "*") {
			m = func(n string) bool { ok, _ := filepath.Match(lq, strings.ToLower(n)); return ok }
		}
		start := time.Now()
		r, err := ix.Search(context.Background(), Query{Text: q, Match: m})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("search %-28q %4d hits (more: %v, partial: %v) in %v", q, len(r.Hits), r.More, r.Partial, time.Since(start).Round(time.Millisecond))
	}
	// Retention forgets the oldest ten.
	start := time.Now()
	if _, err := ix.Reconcile(snaps[10:]); err != nil {
		t.Fatal(err)
	}
	s3 := ix.Status()
	t.Logf("forgetting the 10 oldest backups: %v; %d names left", time.Since(start).Round(time.Millisecond), s3.Names)
}
