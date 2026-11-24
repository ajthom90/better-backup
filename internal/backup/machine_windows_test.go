package backup_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/vss"
)

// diskNumber returns the disk number Windows gave an attached virtual disk.
func diskNumber(t *testing.T, vhd string) string {
	t.Helper()
	out, err := exec.Command("powershell", "-NoProfile", "-Command", "(Get-DiskImage -ImagePath '"+vhd+"').Number").CombinedOutput()
	n := strings.TrimSpace(string(out))
	if _, perr := strconv.Atoi(n); err != nil || perr != nil {
		t.Fatalf("finding the disk number of %s: %v\n%s", vhd, err, out)
	}
	return n
}

func freeLetter(t *testing.T) string {
	t.Helper()
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	for c := 'Z'; c >= 'G'; c-- {
		if mask&(1<<uint(c-'A')) == 0 {
			return string(c)
		}
	}
	t.Fatal("no free drive letter")
	return ""
}

// TestWindowsWholeMachine backs up a whole GPT disk, laid out like a
// Windows system disk with an EFI system partition and an NTFS volume
// read from a VSS snapshot, restores it as a new virtual disk, and
// checks that Windows finds the same partitions and files on it.
func TestWindowsWholeMachine(t *testing.T) {
	if _, err := exec.LookPath("diskpart"); err != nil {
		t.Skip("diskpart is not available")
	}
	vss.EnablePrivileges()
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

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vhdPath := filepath.Join(dir, "system.vhdx")
	letter := freeLetter(t)
	diskpart(t, fmt.Sprintf("create vdisk file=\"%s\" maximum=700 type=expandable\nattach vdisk\nconvert gpt\ncreate partition efi size=100\nformat fs=fat32 quick label=ESP\ncreate partition primary\nformat fs=ntfs quick label=System\nassign letter=%s\n", vhdPath, letter))
	detach := filepath.Join(dir, "detach.txt")
	if err := os.WriteFile(detach, []byte(fmt.Sprintf("select vdisk file=\"%s\"\ndetach vdisk\n", vhdPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	detachOnCleanup(t, vhdPath, detach)
	vol := letter + ":"
	want := map[string][]byte{`boot.txt`: []byte("the system volume"), `data\blob.bin`: rnd(77, 4<<20)}
	for name, data := range want {
		write(t, filepath.Join(vol+`\`, name), data)
	}

	plan, err := backup.PlanMachine([]string{diskNumber(t, vhdPath)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SnapPaths) != 1 || !strings.EqualFold(plan.SnapPaths[0], vol+`\`) {
		plan.Close()
		t.Fatalf("volumes to snapshot: %v, want %s\\", plan.SnapPaths, vol)
	}
	orig := plan.Layout.Disks[0]
	t.Logf("disk: %s", orig.Summary())
	var ntfs, esp *disklayout.Partition
	for i := range orig.Partitions {
		p := &orig.Partitions[i]
		switch {
		case p.Kind == disklayout.KindESP:
			esp = p
		case strings.EqualFold(p.FS, "NTFS"):
			ntfs = p
		}
	}
	if orig.Style != disklayout.StyleGPT || esp == nil || ntfs == nil || esp.Image == "" || ntfs.Image == "" || !strings.EqualFold(ntfs.Mount, vol) {
		plan.Close()
		t.Fatalf("layout: %+v", orig.Partitions)
	}
	set, skipped, err := vss.Create(plan.SnapPaths, 5*time.Minute)
	if err != nil {
		plan.Close()
		t.Fatalf("snapshot: %v (skipped %v)", err, skipped)
	}
	write(t, filepath.Join(vol+`\`, "late.txt"), []byte("written after the snapshot"))
	m, err := plan.Open(set.Path)
	if err != nil {
		set.Close()
		t.Fatal(err)
	}
	sources, stats, err := backup.Run(ctx, &store{r}, backup.Options{IDKey: r.Keys().ID, Machine: m,
		Warn: func(m string) { t.Errorf("warning: %s", m) }})
	set.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disk of %d MiB: read %d MiB, uploaded %d MiB", orig.Size>>20, stats.BytesRead>>20, stats.BytesUploaded>>20)
	if stats.BytesRead > orig.Size/2 {
		t.Errorf("read %d of %d bytes: free space was not skipped", stats.BytesRead, orig.Size)
	}

	// The restored disk has the same identifiers as the original, so the
	// original must be gone before Windows sees the copy.
	diskpart(t, fmt.Sprintf("select vdisk file=\"%s\"\ndetach vdisk\n", vhdPath))
	out := filepath.Join(dir, "out")
	if _, err := restore.Run(ctx, r.Get, restore.Options{Sources: sources, SourceOS: "windows", Target: out}); err != nil {
		t.Fatal(err)
	}
	found, _ := filepath.Glob(filepath.Join(out, "*-disk0.vhdx"))
	if len(found) != 1 {
		t.Fatalf("restored disks: %v", found)
	}
	restored := found[0]
	letter = freeLetter(t)
	detach2 := filepath.Join(dir, "detach2.txt")
	if err := os.WriteFile(detach2, []byte(fmt.Sprintf("select vdisk file=\"%s\"\ndetach vdisk\n", restored)), 0o600); err != nil {
		t.Fatal(err)
	}
	diskpart(t, fmt.Sprintf("select vdisk file=\"%s\"\nattach vdisk\nselect partition %d\nassign letter=%s\n", restored, ntfs.Number, letter))
	detachOnCleanup(t, restored, detach2)

	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(letter+`:\`, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s on the restored disk: %d bytes, %v", name, len(got), err)
		}
	}
	if _, err := os.Stat(filepath.Join(letter+`:\`, "late.txt")); err == nil {
		t.Error("a file written after the snapshot is on the restored disk")
	}
	again, err := backup.PlanMachine([]string{diskNumber(t, restored)})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got := again.Layout.Disks[0]
	if got.GUID != orig.GUID || len(got.Partitions) != len(orig.Partitions) {
		t.Fatalf("restored disk %s with %d partitions, want %s with %d", got.GUID, len(got.Partitions), orig.GUID, len(orig.Partitions))
	}
	for i, p := range orig.Partitions {
		q := got.Partitions[i]
		if p.Offset != q.Offset || p.Size != q.Size || p.Type != q.Type || p.GUID != q.GUID || p.Name != q.Name || !strings.EqualFold(p.FS, q.FS) {
			t.Errorf("partition %d: got %+v, want %+v", p.Number, q, p)
		}
	}
}
