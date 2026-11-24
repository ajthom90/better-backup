package qemuimg

import (
	"bytes"
	"context"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ajthom90/better-backup/internal/nbd"
)

// TestRoundTrip writes a qcow2 image through qemu-nbd and reads it back
// with its allocation map. It needs qemu-utils.
func TestRoundTrip(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "-odd name.qcow2")
	const size = 64<<20 + 4096
	if err := Create(ctx, path, "qcow2", size); err != nil {
		t.Fatal(err)
	}
	w, err := Open(ctx, path, "qcow2", true)
	if err != nil {
		t.Fatal(err)
	}
	if w.Size() != size || w.ReadOnly() {
		t.Fatalf("size %d, read only %v", w.Size(), w.ReadOnly())
	}
	a, b := make([]byte, 3<<20), make([]byte, 4096)
	rand.New(rand.NewSource(1)).Read(a)
	rand.New(rand.NewSource(2)).Read(b)
	if _, err := w.WriteAt(a, 5<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(b, size-4096); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(b, size-1); err == nil {
		t.Fatal("a write past the end was accepted")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "check", path).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img check: %v\n%s", err, out)
	}

	r, err := Open(ctx, path, "qcow2", false, nbd.Allocation, nbd.DirtyBitmap("none"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.ReadOnly() || !r.Has(nbd.Allocation) || r.Has(nbd.DirtyBitmap("none")) {
		t.Fatalf("read only %v, allocation %v", r.ReadOnly(), r.Has(nbd.Allocation))
	}
	got := make([]byte, size)
	if n, err := r.ReadAt(got, 0); err != nil || n != size {
		t.Fatalf("read %d: %v", n, err)
	}
	want := make([]byte, size)
	copy(want[5<<20:], a)
	copy(want[size-4096:], b)
	if !bytes.Equal(got, want) {
		t.Fatal("the image does not read back as written")
	}
	ext, err := r.Status(nbd.Allocation)
	if err != nil {
		t.Fatal(err)
	}
	var data, end int64
	for _, e := range ext {
		if e.Off != end {
			t.Fatalf("extents have a gap at %d", end)
		}
		end += e.Len
		if e.Flags&2 == 0 {
			data += e.Len
			if e.Off+e.Len <= 5<<20 || e.Off >= 8<<20 && e.Off+e.Len <= size-65536 {
				t.Errorf("data reported where nothing was written: %+v", e)
			}
		}
	}
	if end != size || data < 3<<20+4096 || data > 4<<20 {
		t.Fatalf("extents cover %d of %d bytes, %d of data", end, size, data)
	}
	if _, err := r.WriteAt(b, 0); err == nil {
		t.Fatal("a write to a read-only image was accepted")
	}
}
