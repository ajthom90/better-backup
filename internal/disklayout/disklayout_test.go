package disklayout_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ajthom90/better-backup/internal/disklayout"
)

func tool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed", name)
	}
	return p
}

func blank(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func read(t *testing.T, path string) *disklayout.Disk {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	d, err := disklayout.Read(f, fi.Size(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func run(t *testing.T, stdin string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// TestGPT reads a table made by sgdisk, writes it to a larger disk and
// has sgdisk check the result.
func TestGPT(t *testing.T) {
	sgdisk := tool(t, "sgdisk")
	src := blank(t, 64<<20)
	run(t, "", sgdisk, "-U", "11111111-2222-3333-4444-555555555555",
		"-n", "1:2048:+16M", "-t", "1:ef00", "-c", "1:EFI system partition", "-u", "1:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
		"-n", "2:0:+1M", "-t", "2:0c01",
		"-n", "4:0:0", "-t", "4:0700", "-c", "4:Basic data partition", "-A", "4:set:63", src)
	d := read(t, src)
	if d.Style != disklayout.StyleGPT || d.SectorSize != 512 || d.GUID != "11111111-2222-3333-4444-555555555555" || len(d.Partitions) != 3 {
		t.Fatalf("unexpected disk: %+v", d)
	}
	p := d.Partitions
	if p[0].Kind != disklayout.KindESP || p[0].Offset != 1<<20 || p[0].Size != 16<<20 || p[0].Name != "EFI system partition" || p[0].GUID != "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE" {
		t.Fatalf("partition 1: %+v", p[0])
	}
	if p[1].Kind != disklayout.KindMSR || p[2].Number != 4 || p[2].Kind != disklayout.KindData || p[2].Attrs != 1<<63 {
		t.Fatalf("partitions 2 and 4: %+v %+v", p[1], p[2])
	}

	dst := blank(t, 100<<20)
	f, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(f, 100<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out := run(t, "", sgdisk, "-v", dst); !strings.Contains(out, "No problems found") {
		t.Fatalf("sgdisk finds fault with the written table:\n%s", out)
	}
	got := read(t, dst)
	want := *d
	got.Size, want.Size, got.Head, want.Head = 0, 0, nil, nil
	if !reflect.DeepEqual(got, &want) {
		t.Fatalf("the table changed on the way:\n got %+v\nwant %+v", got, want)
	}

	// A disk too small for the last partition is refused.
	if err := d.Write(nopWriter{}, 60<<20); err == nil {
		t.Fatal("writing to a disk that is too small succeeded")
	}
	if err := d.Check(100<<20, 4096); err == nil {
		t.Fatal("a disk with other sectors was accepted")
	}
}

type nopWriter struct{}

func (nopWriter) WriteAt(p []byte, off int64) (int, error) { return len(p), nil }

// TestMBR reads a table with logical partitions made by sfdisk and puts
// it on another disk.
func TestMBR(t *testing.T) {
	sfdisk := tool(t, "sfdisk")
	src := blank(t, 64<<20)
	run(t, "label: dos\nlabel-id: 0x1234abcd\nstart=2048, size=16M, type=7, bootable\nsize=8M, type=83\ntype=5\nsize=4M, type=82\nsize=4M, type=83\n", sfdisk, src)
	// Something a boot loader would keep behind the boot sector.
	f, _ := os.OpenFile(src, os.O_RDWR, 0)
	f.WriteAt([]byte("boot loader stage"), 4096)
	f.Close()
	d := read(t, src)
	if d.Style != disklayout.StyleMBR || len(d.Partitions) != 5 || len(d.EBRs) != 2 || len(d.Head) != 1<<20 {
		t.Fatalf("unexpected disk: style %s, %d partitions, %d EBRs, head %d", d.Style, len(d.Partitions), len(d.EBRs), len(d.Head))
	}
	p := d.Partitions
	if !p[0].Active || p[0].Type != "07" || p[2].Kind != disklayout.KindExtended || p[3].Kind != disklayout.KindSwap || !p[3].Logical || p[3].Number != 5 || p[4].Number != 6 || p[4].Size != 4<<20 {
		t.Fatalf("partitions: %+v", p)
	}
	want := run(t, "", sfdisk, "-d", src)

	dst := blank(t, 80<<20)
	f, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(f, 80<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got := run(t, "", sfdisk, "-d", dst)
	strip := func(s string) string {
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.HasPrefix(l, "device:") {
				keep = append(keep, strings.ReplaceAll(strings.ReplaceAll(l, src, "X"), dst, "X"))
			}
		}
		return strings.Join(keep, "\n")
	}
	if strip(got) != strip(want) {
		t.Fatalf("sfdisk sees another table:\n got %s\nwant %s", got, want)
	}
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(b[4096:4096+17], []byte("boot loader stage")) {
		t.Fatal("the space behind the boot sector was not restored")
	}
}

// TestRoundTrip needs no tools: it writes a table and reads it back.
func TestRoundTrip(t *testing.T) {
	d := &disklayout.Disk{SectorSize: 4096, Style: disklayout.StyleGPT, GUID: "01234567-89AB-CDEF-0123-456789ABCDEF", Entries: 128, FirstUsable: 6,
		Head: make([]byte, 4096),
		Partitions: []disklayout.Partition{
			{Number: 1, Offset: 1 << 20, Size: 8 << 20, Type: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", Kind: disklayout.KindESP, GUID: "00000000-0000-0000-0000-000000000001", Name: "ESP"},
			{Number: 2, Offset: 9 << 20, Size: 20 << 20, Type: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", Kind: disklayout.KindData, GUID: "00000000-0000-0000-0000-000000000002", Name: "root"},
		}}
	copy(d.Head, "boot code")
	path := blank(t, 32<<20)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := d.Write(f, 32<<20); err != nil {
		t.Fatal(err)
	}
	got, err := disklayout.Read(f, 32<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.SectorSize != 4096 || len(got.Partitions) != 2 || got.Partitions[1].Name != "root" || got.Partitions[1].Size != 20<<20 || string(got.Head[:9]) != "boot code" {
		t.Fatalf("read back %+v", got)
	}
	if g, err := disklayout.ParseGUID(d.GUID); err != nil || disklayout.GUIDString(g[:]) != d.GUID {
		t.Fatalf("GUID round trip: %v %v", g, err)
	}
}

func TestSniff(t *testing.T) {
	mkfs := tool(t, "mkfs.ext4")
	path := blank(t, 16<<20)
	run(t, "", mkfs, "-q", "-L", "rootfs", path)
	f, _ := os.Open(path)
	defer f.Close()
	if fs, label := disklayout.Sniff(f); fs != "ext4" || label != "rootfs" {
		t.Fatalf("sniffed %q %q", fs, label)
	}
}
