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
