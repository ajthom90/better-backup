//go:build windows

package backup_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/vss"
)

func dacl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// TestWindowsSnapshotAndPermissions backs up through a VSS snapshot a
// file that is locked and a file with custom permissions, restores both
// elsewhere, and checks content and permissions.
func TestWindowsSnapshotAndPermissions(t *testing.T) {
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

	src := filepath.Join(t.TempDir(), "data")
	write(t, filepath.Join(src, "locked.db"), []byte("locked content"))
	write(t, filepath.Join(src, "plain.txt"), []byte("inherits from its folder"))
	secret := filepath.Join(src, "secret.txt")
	write(t, secret, []byte("restricted"))
	// Users: read. SYSTEM and Administrators: full. No inheritance.
	if out, err := exec.Command("icacls", secret, "/inheritance:r", "/grant:r",
		"*S-1-5-32-545:(R)", "*S-1-5-18:(F)", "*S-1-5-32-544:(F)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	// Alternate data streams on a file and on a folder.
	if err := os.WriteFile(filepath.Join(src, "plain.txt")+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src+":folder-note", []byte("stream on a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := dacl(t, secret)
	if !strings.HasPrefix(want, "D:P") {
		t.Fatalf("test setup: expected a protected DACL, got %s", want)
	}

	p, _ := windows.UTF16PtrFromString(filepath.Join(src, "locked.db"))
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	st := &store{r: r}
	opts := backup.Options{Paths: []string{src}, IDKey: r.Keys().ID}

	// Without a snapshot the locked file is skipped with a warning.
	_, live, err := backup.Run(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if live.Warnings == 0 || live.Files != 2 {
		t.Fatalf("live backup of a locked file: %+v", live)
	}

	set, skipped, err := vss.Create([]string{src}, 5*time.Minute)
	if err != nil {
		if os.Getenv("CI") == "" && strings.Contains(err.Error(), "access denied") {
			t.Skip("not running as administrator: ", err)
		}
		t.Fatalf("vss.Create: %v %v", err, skipped)
	}
	opts.MapPath = set.Path
	sources, stats, err := backup.Run(ctx, st, opts)
	if cerr := set.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || stats.Warnings != 0 || stats.Files != 3 {
		t.Fatalf("snapshot backup: %+v %v", stats, err)
	}
	if sources[0].Name != src {
		t.Fatalf("snapshot paths leaked into the backup: %s", sources[0].Name)
	}

	out := t.TempDir()
	rs, err := restore.Run(ctx, func(id keys.ID) ([]byte, error) { return r.Get(id) },
		restore.Options{Sources: sources, SourceOS: "windows", Target: out})
	if err != nil || rs.Warnings != 0 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	restored := filepath.Join(out, filepath.VolumeName(src)[:1], src[len(filepath.VolumeName(src)):])
	if b, _ := os.ReadFile(filepath.Join(restored, "locked.db")); string(b) != "locked content" {
		t.Fatalf("locked file restored as %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(restored, "plain.txt") + ":Zone.Identifier"); string(b) != "[ZoneTransfer]\r\nZoneId=3\r\n" {
		t.Fatalf("file stream restored as %q (%v)", b, err)
	}
	if b, err := os.ReadFile(restored + ":folder-note"); string(b) != "stream on a directory" {
		t.Fatalf("directory stream restored as %q (%v)", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(restored, "plain.txt")); string(b) != "inherits from its folder" {
		t.Fatalf("main content changed by stream restore: %q", b)
	}
	if got := dacl(t, filepath.Join(restored, "secret.txt")); got != want {
		t.Fatalf("permissions differ after restore:\n got %s\nwant %s", got, want)
	}
	if got := dacl(t, filepath.Join(restored, "plain.txt")); strings.HasPrefix(got, "D:P") {
		t.Fatalf("an inheriting file came back with inheritance blocked: %s", got)
	}
}
