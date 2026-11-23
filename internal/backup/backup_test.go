package backup_test

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

type store struct{ r *repo.Repo }

func (s *store) Missing(_ context.Context, ids []keys.ID) ([]keys.ID, error) { return s.r.Missing(ids) }
func (s *store) Put(_ context.Context, id keys.ID, d []byte) error           { return s.r.Put(id, d) }
func (s *store) Get(_ context.Context, id keys.ID) ([]byte, error)           { return s.r.Get(id) }

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func rnd(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// same fails unless every file under a matches b byte for byte.
func same(t *testing.T, a, b string) {
	t.Helper()
	n := 0
	err := filepath.Walk(a, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(a, p)
		q := filepath.Join(b, rel)
		gi, err := os.Lstat(q)
		if err != nil {
			t.Errorf("missing after restore: %s", rel)
			return nil
		}
		if fi.Mode().Type() != gi.Mode().Type() {
			t.Errorf("%s: type differs", rel)
		}
		if fi.Mode().IsRegular() {
			n++
			x, _ := os.ReadFile(p)
			y, _ := os.ReadFile(q)
			if !bytes.Equal(x, y) {
				t.Errorf("%s: content differs", rel)
			}
			if !fi.ModTime().Equal(gi.ModTime()) {
				t.Errorf("%s: mtime differs", rel)
			}
			if runtime.GOOS != "windows" && fi.Mode() != gi.Mode() {
				t.Errorf("%s: mode %v != %v", rel, fi.Mode(), gi.Mode())
			}
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("compare walked %d files: %v", n, err)
	}
}

func TestBackupRestore(t *testing.T) {
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

	src := filepath.Join(t.TempDir(), "data")
	write(t, filepath.Join(src, "a.txt"), []byte("hello"))
	write(t, filepath.Join(src, "empty"), nil)
	write(t, filepath.Join(src, "sub", "big.bin"), rnd(1, 5<<20))
	write(t, filepath.Join(src, "sub", "copy.bin"), rnd(1, 5<<20))
	write(t, filepath.Join(src, "sub", "deep", "x.log"), []byte("skip me"))
	os.Chmod(filepath.Join(src, "a.txt"), 0o600)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	os.Chtimes(filepath.Join(src, "sub", "big.bin"), old, old)

	opts := backup.Options{Paths: []string{src}, Excludes: []string{"*.log"}, IDKey: r.Keys().ID}
	s1, stats1, err := backup.Run(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats1.Files != 4 || stats1.Warnings != 0 {
		t.Fatalf("first backup stats: %+v", stats1)
	}
	if stats1.BytesUploaded > 6<<20 {
		t.Fatalf("identical files were not deduplicated: uploaded %d", stats1.BytesUploaded)
	}

	// Second run: one file changed, nothing else is read or uploaded.
	write(t, filepath.Join(src, "a.txt"), []byte("hello, changed"))
	opts.Parent = s1
	s2, stats2, err := backup.Run(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.FilesChanged != 1 || stats2.BytesRead != int64(len("hello, changed")) {
		t.Fatalf("incremental stats: %+v", stats2)
	}

	get := func(id keys.ID) ([]byte, error) { return r.Get(id) }

	// Restore the first snapshot elsewhere: the old content comes back.
	out := t.TempDir()
	rs, err := restore.Run(ctx, get, restore.Options{Sources: s1, SourceOS: runtime.GOOS, Target: out})
	if err != nil || rs.Warnings != 0 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	restored := filepath.Join(out, filepath.FromSlash(filepath.ToSlash(src)[len(filepath.VolumeName(src)):]))
	if runtime.GOOS == "windows" {
		restored = filepath.Join(out, filepath.VolumeName(src)[:1], src[len(filepath.VolumeName(src)):])
	}
	if b, _ := os.ReadFile(filepath.Join(restored, "a.txt")); string(b) != "hello" {
		t.Fatalf("snapshot 1 a.txt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(restored, "sub", "deep", "x.log")); err == nil {
		t.Fatal("excluded file was backed up")
	}

	// Restore in place: damage the source, then put snapshot 2 back.
	os.Remove(filepath.Join(src, "sub", "big.bin"))
	write(t, filepath.Join(src, "sub", "copy.bin"), []byte("corrupted"))
	os.Remove(filepath.Join(src, "sub", "deep", "x.log"))
	rs, err = restore.Run(ctx, get, restore.Options{Sources: s2, SourceOS: runtime.GOOS})
	if err != nil || rs.Warnings != 0 {
		t.Fatalf("restore in place: %+v %v", rs, err)
	}
	if rs.Files != 2 {
		t.Fatalf("in-place restore should rewrite exactly the 2 damaged files, wrote %d (skipped %d)", rs.Files, rs.Skipped)
	}
	// Bring snapshot 2 out to a clean directory and compare with the source.
	out2 := t.TempDir()
	if _, err := restore.Run(ctx, get, restore.Options{Sources: s2, SourceOS: runtime.GOOS, Target: out2}); err != nil {
		t.Fatal(err)
	}
	rel := filepath.ToSlash(src)[len(filepath.VolumeName(src)):]
	if runtime.GOOS == "windows" {
		rel = filepath.VolumeName(src)[:1] + rel
	}
	same(t, src, filepath.Join(out2, filepath.FromSlash(rel)))

	// Partial restore of one file.
	out3 := t.TempDir()
	rs, err = restore.Run(ctx, get, restore.Options{Sources: s2, SourceOS: runtime.GOOS, Target: out3,
		Include: []string{filepath.Join(src, "sub", "big.bin")}})
	if err != nil || rs.Files != 1 {
		t.Fatalf("partial restore: %+v %v", rs, err)
	}

	// Everything the snapshots reference survives a GC; verify is clean.
	seen := map[keys.ID]struct{}{}
	if _, err := r.GC(func(mark func(keys.ID) error) error {
		return tree.Mark(get, s2, seen, mark)
	}); err != nil {
		t.Fatal(err)
	}
	out4 := t.TempDir()
	if rs, err := restore.Run(ctx, get, restore.Options{Sources: s2, SourceOS: runtime.GOOS, Target: out4}); err != nil || rs.Warnings != 0 {
		t.Fatalf("restore after gc: %+v %v", rs, err)
	}
}

// TestSystemState backs up only the files a selection accepts, under one
// "System state" entry, and restores them to a folder.
func TestSystemState(t *testing.T) {
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

	root := filepath.Join(t.TempDir(), "drive")
	write(t, filepath.Join(root, "Windows", "config", "SYSTEM"), []byte("hive"))
	write(t, filepath.Join(root, "Windows", "config", "other"), []byte("not wanted"))
	write(t, filepath.Join(root, "Windows", "NTDS", "ntds.dit"), rnd(3, 2<<20))
	write(t, filepath.Join(root, "Users", "a", "doc.txt"), []byte("not system state"))
	match := func(p string, dir bool) bool {
		rel, _ := filepath.Rel(root, p)
		switch filepath.ToSlash(rel) {
		case "Windows", "Windows/config", "Windows/NTDS", "Windows/config/SYSTEM", "Windows/NTDS/ntds.dit":
			return true
		}
		return false
	}
	opts := backup.Options{SystemState: &backup.SystemState{Roots: []string{root}, Match: match}, IDKey: r.Keys().ID}
	s1, stats, err := backup.Run(ctx, st, opts)
	if err != nil || stats.Files != 2 || stats.Warnings != 0 {
		t.Fatalf("backup: %+v %v", stats, err)
	}
	if len(s1) != 1 || s1[0].Name != tree.SystemStateName || s1[0].Type != tree.TypeDir {
		t.Fatalf("sources: %+v", s1)
	}

	// Unchanged files are not read again.
	opts.Parent = s1
	_, stats, err = backup.Run(ctx, st, opts)
	if err != nil || stats.Files != 2 || stats.FilesChanged != 0 || stats.BytesRead != 0 {
		t.Fatalf("second backup: %+v %v", stats, err)
	}

	get := func(id keys.ID) ([]byte, error) { return r.Get(id) }
	out := t.TempDir()
	rs, err := restore.Run(ctx, get, restore.Options{Sources: s1, SourceOS: runtime.GOOS, Target: out})
	if err != nil || rs.Warnings != 0 || rs.Files != 2 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	drive := "drive"
	if v := filepath.VolumeName(root); v != "" {
		drive = v[:1] // a root on Windows is filed under its drive letter
	}
	base := filepath.Join(out, tree.SystemStateName, drive, "Windows")
	if b, _ := os.ReadFile(filepath.Join(base, "config", "SYSTEM")); string(b) != "hive" {
		t.Fatalf("restored hive = %q", b)
	}
	if _, err := os.Stat(filepath.Join(base, "config", "other")); err == nil {
		t.Fatal("a file outside the selection was backed up")
	}
	if _, err := os.Stat(filepath.Join(out, tree.SystemStateName, drive, "Users")); err == nil {
		t.Fatal("a folder outside the selection was backed up")
	}

	// In place it is refused with a warning, and nothing is written.
	rs, err = restore.Run(ctx, get, restore.Options{Sources: s1, SourceOS: runtime.GOOS})
	if err != nil || rs.Warnings != 1 || rs.Files != 0 {
		t.Fatalf("in-place restore: %+v %v", rs, err)
	}

	// With nothing readable the backup fails instead of saving an empty state.
	opts = backup.Options{SystemState: &backup.SystemState{Roots: []string{root}, Match: func(string, bool) bool { return false }}, IDKey: r.Keys().ID}
	if _, _, err := backup.Run(ctx, st, opts); err == nil {
		t.Fatal("an empty system state was accepted")
	}
}
