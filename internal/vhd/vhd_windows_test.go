package vhd

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateWriteRead makes a virtual disk, writes to it, and reads the
// contents back through a second, read-only open.
func TestCreateWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.vhdx")
	const size = 256 << 20 // eight 32 MiB blocks, of which two are written
	d, err := Create(path, size)
	if err != nil {
		t.Fatal(err)
	}
	if d.Size() != size {
		t.Fatalf("size %d, want %d", d.Size(), size)
	}
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(3)).Read(data)
	for _, off := range []int64{0, 17 << 20, size - 1<<20} {
		if n, err := d.WriteAt(data[1:1<<20-511], off+512); err != nil { // from an odd address
			t.Fatalf("write at %d: %d %v", off, n, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() >= size {
		t.Fatalf("the file should be smaller than the disk it holds: %v %v", fi, err)
	}

	d, err = Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Size() != size {
		t.Fatalf("size after reopen %d, want %d", d.Size(), size)
	}
	got := make([]byte, 1<<20)
	if _, err := d.ReadAt(got, 17<<20); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 1<<20)
	copy(want[512:], data[1:1<<20-511])
	if !bytes.Equal(got, want) {
		t.Fatal("data read back differs from what was written")
	}
	if _, err := d.ReadAt(got, 5<<20); err != nil || !bytes.Equal(got, make([]byte, 1<<20)) {
		t.Fatalf("an untouched part of the disk should read as zeros: %v", err)
	}
	if _, err := d.WriteAt(data, 0); err == nil {
		t.Fatal("a read-only disk accepted a write")
	}
}

// TestTracking switches change tracking on for a disk, writes to it and
// asks the disk what changed.
func TestTracking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.vhdx")
	const size = 256 << 20
	d, err := Create(path, size)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(4)).Read(data)
	if _, err := d.WriteAt(data, 3<<20); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if id, err := Tracking(path); err != nil || id != "" {
		t.Fatalf("a new disk should not track changes: %q %v", id, err)
	}
	if _, err := Changes(path, "rctX:00000000:00000000:00000000-0000-0000-0000-000000000000:00000000"); err == nil {
		t.Fatal("a disk without change tracking listed changes")
	}
	if err := SetTracking(path, true); err != nil {
		t.Fatal(err)
	}
	id, err := Tracking(path)
	if err != nil || id == "" {
		t.Fatalf("no tracking id after switching tracking on: %q %v", id, err)
	}
	t.Logf("tracking id: %s", id)
	if ch, err := Changes(path, id); err != nil || len(ch) != 0 {
		t.Fatalf("nothing was written yet, got %v %v", ch, err)
	}

	if d, err = Open(path, false); err != nil {
		t.Fatal(err)
	}
	const at = 100 << 20
	if _, err := d.WriteAt(data, at); err != nil {
		t.Fatal(err)
	}
	d.Close()
	ch, err := Changes(path, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("changes: %v", ch)
	var total int64
	covered := false
	for _, r := range ch {
		total += r.Len
		if r.Off <= at && r.Off+r.Len >= at+1<<20 {
			covered = true
		}
		if r.Off < 3<<20+1<<20 && r.Off+r.Len > 3<<20 {
			t.Fatalf("a part written before tracking began is listed: %v", r)
		}
	}
	if !covered || total > 64<<20 {
		t.Fatalf("the write at %d should be listed and little else: %v", at, ch)
	}
	id2, err := Tracking(path)
	t.Logf("tracking id after the write: %s %v", id2, err)
	if _, err := Changes(path, "rctX:deadbeef:00000000:00000000-0000-0000-0000-000000000000:00000000"); err == nil {
		t.Fatal("an unknown tracking id listed changes")
	}
}
