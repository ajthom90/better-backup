package vmdk

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const block = 1 << 20

// disk makes a disk image with data in some blocks and which blocks
// those are. The size is not a whole number of blocks or grains.
func disk(size int64) ([]byte, []bool) {
	raw := make([]byte, size)
	rng := rand.New(rand.NewSource(size))
	data := make([]bool, (size+block-1)/block)
	for _, b := range []int64{0, 3, 4, 31, 32, 33, 70, int64(len(data)) - 1} {
		if b < int64(len(data)) && !data[b] {
			data[b] = true
			end := min((b+1)*block, size)
			rng.Read(raw[b*block : end])
			// Leave part of the block empty, as real disks have it.
			if from := b*block + 100_000; from < end {
				clear(raw[from:min(from+200_000, end)])
			}
		}
	}
	return raw, data
}

func encode(t *testing.T, raw []byte, data []bool) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, int64(len(raw)), block, data)
	if err != nil {
		t.Fatal(err)
	}
	for b, has := range data {
		if has {
			off := int64(b) * block
			if _, err := w.WriteAt(raw[off:min(off+block, int64(len(raw)))], off); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := StreamSize(int64(len(raw)), block, data)
	if err != nil || want != int64(out.Len()) {
		t.Fatalf("StreamSize says %d (%v), the stream has %d bytes", want, err, out.Len())
	}
	return out.Bytes()
}

// decode reads a stream the way a backup does: block by block.
func decode(t *testing.T, stream []byte) []byte {
	t.Helper()
	r, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, r.Size())
	for off := int64(0); off < r.Size(); off += block {
		if _, err := r.ReadAt(out[off:min(off+block, r.Size())], off); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	for _, size := range []int64{512, 65536, 3*block + 4096, 80*block + 512*77} {
		raw, data := disk(size)
		stream := encode(t, raw, data)
		if got := decode(t, stream); !bytes.Equal(got, raw) {
			t.Fatalf("a disk of %d bytes does not read back as written", size)
		}
	}
	// Nothing but holes.
	stream := encode(t, make([]byte, 5*block), make([]bool, 5))
	if got := decode(t, stream); !bytes.Equal(got, make([]byte, 5*block)) {
		t.Fatal("an empty disk does not read back empty")
	}
}

func TestErrors(t *testing.T) {
	raw, data := disk(40 * block)
	stream := encode(t, raw, data)
	r, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, block)
	if _, err := r.ReadAt(buf, 5*block); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAt(buf, 4*block); err == nil {
		t.Fatal("reading backwards was accepted")
	}
	// A stream cut short is an error, not a disk that ends in zeros.
	r, _ = NewReader(bytes.NewReader(stream[:len(stream)/2]))
	var rerr error
	for off := int64(0); off < r.Size() && rerr == nil; off += block {
		_, rerr = r.ReadAt(buf, off)
	}
	if rerr == nil {
		rerr = r.Drain()
	}
	if rerr == nil {
		t.Fatal("a truncated stream was read without an error")
	}
	if _, err := NewReader(bytes.NewReader(make([]byte, 4096))); err == nil {
		t.Fatal("zeros were accepted as a VMDK")
	}

	var out bytes.Buffer
	w, _ := NewWriter(&out, 4*block, block, []bool{true, false, true, false})
	if _, err := w.WriteAt(raw[:block], block); err == nil {
		t.Fatal("a write to a block without data was accepted")
	}
	w, _ = NewWriter(&out, 4*block, block, []bool{true, false, true, false})
	w.WriteAt(raw[:block], 0)
	if err := w.Close(); err == nil {
		t.Fatal("a disk with a block missing was closed without an error")
	}
	if _, err := NewWriter(&out, 4*block, 1000, make([]bool, 4)); err == nil {
		t.Fatal("blocks that do not line up with grains were accepted")
	}
}

// TestQEMU checks both directions against QEMU's implementation of
// the format: it reads what the Writer wrote, and the Reader reads
// what it wrote, which is really compressed.
func TestQEMU(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is not installed")
	}
	dir := t.TempDir()
	raw, data := disk(80*block + 512*77)
	rawFile, mine, theirs := filepath.Join(dir, "disk.raw"), filepath.Join(dir, "mine.vmdk"), filepath.Join(dir, "theirs.vmdk")
	if err := os.WriteFile(rawFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, encode(t, raw, data), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("qemu-img", args...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img %v: %v\n%s", args, err, out)
		}
	}
	run("compare", "-f", "vmdk", "-F", "raw", mine, rawFile)
	run("check", "-f", "vmdk", mine)
	run("convert", "-f", "raw", "-O", "vmdk", "-o", "subformat=streamOptimized", rawFile, theirs)
	stream, err := os.ReadFile(theirs)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(t, stream); !bytes.Equal(got, raw) {
		t.Fatal("a disk written by qemu-img does not read back right")
	}
	t.Log(fmt.Sprintf("raw %d bytes, stored grains %d bytes, qemu's compressed %d bytes", len(raw), len(encode(t, raw, data)), len(stream)))
}
