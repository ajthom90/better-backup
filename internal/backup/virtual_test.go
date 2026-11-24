package backup_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

// vfs is a tree in memory, served as a virtual source. A nil value is
// a file that cannot be opened; "cut:" in front makes its reading fail.
type vfs struct {
	files   map[string][]byte // "dir/name" to content
	keys    map[string]string
	broken  map[string]bool // folders that cannot be listed
	opens   atomic.Int64
	current atomic.Int64
	most    atomic.Int64
}

type cutReader struct{ r io.Reader }

func (c cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		err = errors.New("the connection was cut")
	}
	return n, err
}

type counted struct {
	io.Reader
	done func()
}

func (c counted) Close() error { c.done(); return nil }

func (v *vfs) node(dir string) *backup.VNode {
	return &backup.VNode{Name: filepath.Base(dir), Dir: true, List: func(context.Context) ([]*backup.VNode, error) {
		if v.broken[dir] {
			return nil, errors.New("the service is away")
		}
		var out []*backup.VNode
		seen := map[string]bool{}
		for path, data := range v.files {
			rest, ok := strings.CutPrefix(path, dir+"/")
			if !ok {
				continue
			}
			name, _, deeper := strings.Cut(rest, "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			if deeper {
				out = append(out, v.node(dir+"/"+name))
				continue
			}
			out = append(out, &backup.VNode{Name: name, Key: v.keys[path], ModTime: time.Unix(1700000000, 0), Open: func(context.Context) (io.ReadCloser, error) {
				v.opens.Add(1)
				if data == nil {
					return nil, errors.New("no such thing")
				}
				if n := v.current.Add(1); n > v.most.Load() {
					v.most.Store(n)
				}
				var r io.Reader = bytes.NewReader(data)
				if bytes.HasPrefix(data, []byte("cut:")) {
					r = cutReader{r}
				}
				return counted{r, func() { v.current.Add(-1) }}, nil
			}})
		}
		return out, nil
	}}
}

func TestVirtual(t *testing.T) {
	ctx := context.Background()
	rdir := t.TempDir()
	if err := repo.Init(rdir, []byte("pw")); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(rdir, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st := &store{r: r}
	get := func(id keys.ID) ([]byte, error) { return r.Get(id) }

	v := &vfs{files: map[string][]byte{}, keys: map[string]string{}, broken: map[string]bool{}}
	// More small files than are downloaded ahead, one far larger than
	// is held in memory, and one exactly that size.
	for i := 0; i < 40; i++ {
		p := fmt.Sprintf("root/many/%02d.txt", i)
		v.files[p], v.keys[p] = []byte(fmt.Sprint("file ", i)), "v1"
	}
	v.files["root/big/huge.bin"], v.keys["root/big/huge.bin"] = rnd(7, 11<<20), "v1"
	v.files["root/big/edge.bin"], v.keys["root/big/edge.bin"] = rnd(8, 4<<20), "v1"
	v.files["root/big/empty"], v.keys["root/big/empty"] = []byte{}, "v1"
	v.files["root/nokey.txt"] = []byte("always read")
	v.files["root/gone.txt"] = nil
	v.files["root/a/b/c/deep.txt"], v.keys["root/a/b/c/deep.txt"] = []byte("deep"), "v1"

	var warnings []string
	run := func(parent []tree.Entry) ([]tree.Entry, tree.Stats) {
		t.Helper()
		warnings = nil
		v.opens.Store(0)
		src, stats, err := backup.Run(ctx, st, backup.Options{IDKey: r.Keys().ID, Virtual: []backup.VirtualSource{{Name: "Cloud", Root: v.node("root")}}, Parent: parent,
			Warn: func(s string) { warnings = append(warnings, s) }})
		if err != nil {
			t.Fatal(err)
		}
		if v.current.Load() != 0 {
			t.Fatalf("%d downloads were left open", v.current.Load())
		}
		return src, stats
	}
	check := func(src []tree.Entry, want map[string][]byte) {
		t.Helper()
		out := t.TempDir()
		if _, err := restore.Run(ctx, get, restore.Options{Sources: src, SourceOS: runtime.GOOS, Target: out}); err != nil {
			t.Fatal(err)
		}
		n := 0
		filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				n++
			}
			return nil
		})
		if n != len(want) {
			t.Fatalf("%d files restored, want %d", n, len(want))
		}
		for p, data := range want {
			got, err := os.ReadFile(filepath.Join(out, "Cloud", filepath.FromSlash(strings.TrimPrefix(p, "root/"))))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s: %d bytes, want %d (%v)", p, len(got), len(data), err)
			}
		}
	}
	want := map[string][]byte{}
	for p, d := range v.files {
		if d != nil {
			want[p] = d
		}
	}

	s1, stats := run(nil)
	if len(s1) != 1 || s1[0].Name != "Cloud" || stats.Files != 45 || stats.FilesChanged != 45 || len(warnings) != 1 || !strings.Contains(warnings[0], "Cloud/gone.txt: no such thing") {
		t.Fatalf("first backup: %+v %v", stats, warnings)
	}
	if v.most.Load() < 2 || v.most.Load() > 12 {
		t.Fatalf("%d downloads were open at once", v.most.Load())
	}
	check(s1, want)

	// Nothing changed: only the files without a key are opened.
	s2, stats := run(s1)
	if stats.Files != 45 || stats.FilesChanged != 1 || v.opens.Load() != 2 || stats.BytesUploaded > 4096 {
		t.Fatalf("second backup: %+v, %d opened", stats, v.opens.Load())
	}
	check(s2, want)

	// A folder that cannot be listed, a file that cannot be opened and
	// one whose download breaks half way all stay as they were. A file
	// that changed is stored, one that is gone is left out.
	v.broken["root/many"] = true
	v.files["root/big/edge.bin"], v.keys["root/big/edge.bin"] = nil, "v2"
	v.files["root/big/huge.bin"], v.keys["root/big/huge.bin"] = append([]byte("cut:"), rnd(9, 6<<20)...), "v2"
	v.files["root/a/b/c/deep.txt"], v.keys["root/a/b/c/deep.txt"] = []byte("deeper"), "v2"
	delete(v.files, "root/big/empty")
	delete(want, "root/big/empty")
	want["root/a/b/c/deep.txt"] = []byte("deeper")
	s3, stats := run(s2)
	if len(warnings) != 4 || stats.FilesChanged != 2 {
		t.Fatalf("third backup: %+v\n%s", stats, strings.Join(warnings, "\n"))
	}
	all := strings.Join(warnings, "\n")
	for _, w := range []string{"Cloud/many: the service is away; kept as it was", "Cloud/big/edge.bin: no such thing; kept as it was", "Cloud/big/huge.bin: the connection was cut"} {
		if !strings.Contains(all, w) {
			t.Fatalf("no warning %q in:\n%s", w, all)
		}
	}
	check(s3, want)

	// What was kept is tried again, and its changes are then stored.
	v.broken = map[string]bool{}
	v.files["root/big/edge.bin"] = []byte("small now")
	v.files["root/big/huge.bin"] = []byte("small too")
	delete(v.files, "root/gone.txt")
	want["root/big/edge.bin"], want["root/big/huge.bin"] = v.files["root/big/edge.bin"], v.files["root/big/huge.bin"]
	s4, stats := run(s3)
	if len(warnings) != 0 || stats.FilesChanged != 3 {
		t.Fatalf("fourth backup: %+v %v", stats, warnings)
	}
	check(s4, want)

	// Stopping a backup stops its downloads.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := backup.Run(cctx, st, backup.Options{IDKey: r.Keys().ID, Virtual: []backup.VirtualSource{{Name: "Cloud", Root: v.node("root")}}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled backup: %v", err)
	}
}
