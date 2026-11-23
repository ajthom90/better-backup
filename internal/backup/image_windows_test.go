package backup_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/vss"
)

func diskpart(t *testing.T, script string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "script.txt")
	if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("diskpart", "/s", f).CombinedOutput(); err != nil {
		t.Fatalf("diskpart: %v\n%s\n%s", err, script, out)
	}
}

// newVolume creates a virtual disk with one NTFS volume and returns its
// drive letter, such as "Q:".
func newVolume(t *testing.T, sizeMB int) string {
	t.Helper()
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	letter := ""
	for c := 'Z'; c >= 'G'; c-- {
		if mask&(1<<uint(c-'A')) == 0 {
			letter = string(c)
			break
		}
	}
	if letter == "" {
		t.Fatal("no free drive letter")
	}
	vhd := filepath.Join(t.TempDir(), "disk.vhdx")
	diskpart(t, fmt.Sprintf("create vdisk file=\"%s\" maximum=%d type=expandable\nattach vdisk\ncreate partition primary\nformat fs=ntfs quick\nassign letter=%s\n", vhd, sizeMB, letter))
	// The detach script is written now: cleanup runs after the test, when
	// no new temporary files may be made.
	detach := filepath.Join(filepath.Dir(vhd), "detach.txt")
	if err := os.WriteFile(detach, []byte(fmt.Sprintf("select vdisk file=\"%s\"\ndetach vdisk\n", vhd)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, err := exec.Command("diskpart", "/s", detach).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "already detached") {
			t.Errorf("detaching %s: %v\n%s", vhd, err, out)
		}
		// The disk service lets go of the file a moment after detaching.
		for i := 0; i < 20; i++ {
			if err := os.Remove(vhd); err == nil || os.IsNotExist(err) {
				break
			}
			time.Sleep(time.Second)
		}
	})
	return letter + ":"
}

// TestWindowsVolumeImage images a real NTFS volume from a VSS snapshot,
// skipping its free space, and restores the image over a second volume.
func TestWindowsVolumeImage(t *testing.T) {
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

	const sizeMB = 1024
	src := newVolume(t, sizeMB)
	want := map[string][]byte{`report.txt`: []byte("quarterly numbers"), `data\big.bin`: rnd(21, 5<<20), `data\deep\note.txt`: []byte("nested")}
	for name, data := range want {
		write(t, filepath.Join(src+`\`, name), data)
	}

	set, skipped, err := vss.Create([]string{src + `\`}, 5*time.Minute)
	if err != nil {
		t.Fatalf("snapshot: %v (skipped %v)", err, skipped)
	}
	// Changes after the snapshot must not reach the image.
	write(t, filepath.Join(src+`\`, "late.txt"), []byte("written after the snapshot"))
	dev := strings.TrimRight(set.Path(src+`\`), `\`)
	img, err := backup.OpenImage(src, dev)
	if err != nil {
		set.Close()
		t.Fatal(err)
	}
	if img.Size < 900<<20 || img.Used == nil {
		t.Errorf("image source: size %d, allocation map present: %v", img.Size, img.Used != nil)
	}
	sources, stats, err := backup.Run(ctx, &store{r}, backup.Options{IDKey: r.Keys().ID, Images: []backup.ImageSource{img},
		Warn: func(m string) { t.Errorf("warning: %s", m) }})
	set.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("volume of %d MiB: read %d MiB, uploaded %d MiB", img.Size>>20, stats.BytesRead>>20, stats.BytesUploaded>>20)
	if stats.BytesRead > img.Size/2 {
		t.Errorf("read %d of %d bytes: free space was not skipped", stats.BytesRead, img.Size)
	}

	// Restore over a second, freshly formatted volume and read the files.
	dst := newVolume(t, sizeMB)
	write(t, filepath.Join(dst+`\`, "stale.txt"), []byte("must be gone after the restore"))
	rs, err := restore.Run(ctx, r.Get, restore.Options{Sources: sources, SourceOS: "windows", Target: dst})
	if err != nil || rs.Files != 1 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(dst+`\`, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s after restore: %d bytes, %v", name, len(got), err)
		}
	}
	for _, gone := range []string{"stale.txt", "late.txt"} {
		if _, err := os.Stat(filepath.Join(dst+`\`, gone)); err == nil {
			t.Errorf("%s exists on the restored volume", gone)
		}
	}
}
