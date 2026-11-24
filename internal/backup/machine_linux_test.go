package backup_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestMachine backs up a whole disk (a file standing in for one) with a
// GPT, an EFI system partition, reserved space, swap and a data
// partition, and restores it as a new disk that must have the same
// table and the same contents.
func TestMachine(t *testing.T) {
	sgdisk, err := exec.LookPath("sgdisk")
	if err != nil {
		t.Skip("sgdisk is not installed")
	}
	const mib = 1 << 20
	src := filepath.Join(t.TempDir(), "disk.img")
	disk := make([]byte, 40*mib)
	copy(disk[1*mib:], rnd(1, 3*mib))  // partition 1: EFI system, with a block of zeros behind the data
	copy(disk[9*mib:], rnd(2, 2*mib))  // partition 2: reserved; never read
	copy(disk[11*mib:], rnd(3, 4*mib)) // partition 3: swap; only its first MiB is kept
	copy(disk[15*mib:], rnd(4, 20*mib))
	write(t, src, disk)
	if out, err := exec.Command(sgdisk, "-n", "1:2048:+8M", "-t", "1:ef00", "-n", "2:0:+2M", "-t", "2:0c01",
		"-n", "3:0:+4M", "-t", "3:8200", "-n", "4:0:+20M", "-t", "4:8300", "-c", "4:root", src).CombinedOutput(); err != nil {
		t.Fatalf("sgdisk: %v\n%s", err, out)
	}
	disk, _ = os.ReadFile(src)

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

	plan, err := backup.PlanMachine([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Layout.Disks) != 1 || len(plan.Layout.Disks[0].Partitions) != 4 || len(plan.SnapPaths) != 0 {
		t.Fatalf("unexpected plan: %+v", plan.Layout)
	}
	p := plan.Layout.Disks[0].Partitions
	if p[0].Image != "disk0-part1" || p[1].Image != "" || p[2].Image != "disk0-part3" || p[3].Image != "disk0-part4" {
		t.Fatalf("images: %q %q %q %q", p[0].Image, p[1].Image, p[2].Image, p[3].Image)
	}
	m, err := plan.Open(func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	sources, stats, err := backup.Run(ctx, &store{r}, backup.Options{Machine: m, IDKey: r.Keys().ID, Warn: func(w string) { t.Errorf("warning: %s", w) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Type != tree.TypeMachine || sources[0].Name != tree.MachineName || stats.Files != 1 {
		t.Fatalf("unexpected sources: %+v, stats %+v", sources, stats)
	}
	// 3 MiB of the EFI partition, 1 MiB of swap and all of the data
	// partition hold something; the rest is never stored.
	if want := int64(24 * mib); stats.BytesUploaded < want || stats.BytesUploaded > want+2*mib {
		t.Fatalf("uploaded %d bytes, want about %d", stats.BytesUploaded, want)
	}
	// Everything is reachable for garbage collection.
	if err := tree.Mark(r.Get, sources, map[keys.ID]struct{}{}, func(id keys.ID) error { _, err := r.Get(id); return err }); err != nil {
		t.Fatal(err)
	}
	layout, images, err := restore.ReadMachine(r.Get, &sources[0])
	if err != nil || len(images) != 3 || layout.Disks[0].Style != disklayout.StyleGPT {
		t.Fatalf("read back: %+v %d %v", layout, len(images), err)
	}

	check := func(path string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(disk) {
			t.Fatalf("%s has %d bytes, want %d", path, len(got), len(disk))
		}
		for _, same := range [][2]int{{0, 9 * mib}, {11 * mib, 12 * mib}, {15 * mib, 35 * mib}, {39 * mib, 40 * mib}} {
			if !bytes.Equal(got[same[0]:same[1]], disk[same[0]:same[1]]) {
				for k := same[0]; k < same[1]; k++ {
					if got[k] != disk[k] {
						t.Fatalf("%s differs from the original at offset %d", path, k)
					}
				}
			}
		}
		if !bytes.Equal(got[9*mib:11*mib], make([]byte, 2*mib)) || !bytes.Equal(got[12*mib:15*mib], make([]byte, 3*mib)) {
			t.Fatalf("%s: space that holds nothing was written", path)
		}
		if out, err := exec.Command(sgdisk, "-v", path).CombinedOutput(); err != nil || !bytes.Contains(out, []byte("No problems found")) {
			t.Fatalf("sgdisk finds fault with %s: %v\n%s", path, err, out)
		}
	}
	// Into a folder: one file per disk, named after the machine.
	dir := t.TempDir()
	rs, err := restore.Run(ctx, r.Get, restore.Options{Sources: sources, SourceOS: "linux", Target: dir})
	if err != nil || rs.Files != 1 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	check(filepath.Join(dir, layout.Hostname+"-disk0.img"))
	// To a chosen place per disk, as the recovery disc does.
	dest := filepath.Join(t.TempDir(), "new-disk.img")
	if _, err := restore.Run(ctx, r.Get, restore.Options{Sources: sources, SourceOS: "linux", Disks: map[string]string{"disk0": dest}}); err != nil {
		t.Fatal(err)
	}
	check(dest)
	// One partition out of the machine, as an image.
	one := filepath.Join(t.TempDir(), "root.img")
	if _, err := restore.Run(ctx, r.Get, restore.Options{Sources: sources, SourceOS: "linux", Include: []string{tree.MachineName + "/disk0-part4"}, Target: one}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(one); !bytes.Equal(got, disk[15*mib:35*mib]) {
		t.Fatal("the partition restored on its own differs")
	}
	// No place to put it: refused with an explanation.
	if _, err := restore.Run(ctx, r.Get, restore.Options{Sources: sources, SourceOS: "linux", Disks: map[string]string{"disk1": dest}}); err == nil {
		t.Fatal("a restore with no disk chosen succeeded")
	}
}
