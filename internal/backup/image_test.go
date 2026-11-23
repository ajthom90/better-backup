package backup_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestImage backs up a disk image with unallocated ranges, checks that
// those are neither read nor stored, backs it up again after a small
// change, and restores it.
func TestImage(t *testing.T) {
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
	st := &store{r}
	const mib = 1 << 20
	disk := rnd(11, 5*mib+4096) // the last block is short
	// Block 0 and a little of block 1 are in use, block 2 is free, and
	// everything from block 3 on is in use.
	used := []backup.Extent{{Off: 0, Len: mib + 100}, {Off: 3 * mib, Len: int64(len(disk)) - 3*mib}}
	run := func(parent []tree.Entry) ([]tree.Entry, tree.Stats) {
		t.Helper()
		closed := false
		src, stats, err := backup.Run(ctx, st, backup.Options{Parent: parent, IDKey: r.Keys().ID,
			Images: []backup.ImageSource{{Name: "/dev/test", Size: int64(len(disk)), R: bytes.NewReader(disk), Used: used, Close: func() { closed = true }}},
			Warn:   func(m string) { t.Errorf("warning: %s", m) }})
		if err != nil {
			t.Fatal(err)
		}
		if !closed {
			t.Error("the image source was not closed")
		}
		return src, stats
	}
	src, stats := run(nil)
	if len(src) != 1 || src[0].Type != tree.TypeImage || src[0].Size != int64(len(disk)) || len(src[0].Content) != 6 {
		t.Fatalf("unexpected image entry: %+v", src)
	}
	if src[0].Content[2] != (keys.ID{}) || src[0].Content[1] == (keys.ID{}) {
		t.Fatalf("holes are wrong: %v", src[0].Content)
	}
	if want := int64(len(disk)) - mib; stats.BytesRead != want || stats.BytesUploaded != want {
		t.Fatalf("read %d and uploaded %d bytes, want %d", stats.BytesRead, stats.BytesUploaded, want)
	}

	// One changed block is the only upload of the next backup.
	old := append([]byte(nil), disk...)
	copy(disk[4*mib+10:], "changed")
	src2, stats2 := run(src)
	if stats2.BytesUploaded != mib {
		t.Fatalf("second backup uploaded %d bytes, want one block", stats2.BytesUploaded)
	}

	// Every chunk of both images is reachable for garbage collection.
	n := 0
	if err := tree.Mark(r.Get, append(src, src2...), map[keys.ID]struct{}{}, func(id keys.ID) error {
		n++
		_, err := r.Get(id)
		return err
	}); err != nil || n != 10 {
		t.Fatalf("marked %d chunks: %v", n, err)
	}

	check := func(path string, want []byte) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s has %d bytes, want %d", path, len(got), len(want))
		}
		if !bytes.Equal(got[:2*mib], want[:2*mib]) || !bytes.Equal(got[3*mib:], want[3*mib:]) {
			t.Fatalf("%s: the allocated blocks differ from the original", path)
		}
		if !bytes.Equal(got[2*mib:3*mib], make([]byte, mib)) {
			t.Fatalf("%s: the unallocated block was written", path)
		}
	}
	// Into a directory: the file is named after the image.
	dir := t.TempDir()
	rs, err := restore.Run(ctx, r.Get, restore.Options{Sources: src2, SourceOS: runtime.GOOS, Target: dir})
	if err != nil || rs.Files != 1 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	check(filepath.Join(dir, "dev_test.img"), disk)
	// To a named file, from the older snapshot.
	file := filepath.Join(t.TempDir(), "old.img")
	if _, err := restore.Run(ctx, r.Get, restore.Options{Sources: src, SourceOS: runtime.GOOS, Target: file}); err != nil {
		t.Fatal(err)
	}
	check(file, old)
}

// TestVM backs up a virtual machine made of a configuration and two
// disks, lists it, and restores one disk out of it.
func TestVM(t *testing.T) {
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
	const mib = 1 << 20
	sys := append(rnd(41, mib), make([]byte, 2*mib)...) // one block of data, two of zeros
	data := rnd(42, 2*mib)
	opened, closed := 0, 0
	disk := func(name string, b []byte) backup.ImageSource {
		return backup.ImageSource{Name: name, Size: int64(len(b)), R: bytes.NewReader(b), SkipZero: true, Close: func() { closed++ }}
	}
	vm := backup.VMSource{Name: "web01", Open: func(v *backup.VMSource) error {
		opened++
		v.ConfigName, v.Config = "vm.json", []byte(`{"name":"web01","generation":2}`)
		v.Disks = []backup.ImageSource{disk("disk-scsi-0-0.vhdx", sys), disk("disk-scsi-0-1.vhdx", data)}
		v.Close = func() { closed++ }
		return nil
	}}
	broken := backup.VMSource{Name: "gone", Open: func(*backup.VMSource) error { return os.ErrNotExist }}
	var warnings []string
	src, stats, err := backup.Run(ctx, &store{r}, backup.Options{IDKey: r.Keys().ID, VMs: []backup.VMSource{vm, broken},
		Warn: func(m string) { warnings = append(warnings, m) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(src) != 1 || src[0].Type != tree.TypeVM || src[0].Name != "web01" || opened != 1 || closed != 3 {
		t.Fatalf("sources %+v, opened %d, closed %d", src, opened, closed)
	}
	if len(warnings) != 1 || stats.Files != 1 || stats.BytesUploaded > 3*mib+1024 {
		t.Fatalf("warnings %v, stats %+v", warnings, stats)
	}
	children, err := tree.ReadPage(r.Get, src[0].Subtree[0])
	if err != nil || len(children) != 3 || children[0].Name != "vm.json" || !children[1].HolesZero {
		t.Fatalf("machine contents: %+v %v", children, err)
	}
	if err := tree.Mark(r.Get, src, map[keys.ID]struct{}{}, func(id keys.ID) error { _, err := r.Get(id); return err }); err != nil {
		t.Fatal(err)
	}

	// One disk, picked by path, restores to a file anywhere.
	sep := "/"
	if runtime.GOOS == "windows" {
		sep = `\`
	}
	out := filepath.Join(t.TempDir(), "system.img")
	rs, err := restore.Run(ctx, r.Get, restore.Options{Sources: src, SourceOS: runtime.GOOS, Target: out, Include: []string{"web01" + sep + "disk-scsi-0-0.vhdx"}})
	if err != nil || rs.Files != 1 {
		t.Fatalf("disk restore: %+v %v", rs, err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, sys) {
		t.Fatal("the restored disk differs from the original")
	}
	// The whole machine needs a target folder, and a Hyper-V host.
	if _, err := restore.Run(ctx, r.Get, restore.Options{Sources: src, SourceOS: runtime.GOOS}); err == nil {
		t.Fatal("a machine was restored without a target folder")
	}
}
