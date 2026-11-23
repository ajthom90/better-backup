//go:build windows

package vss

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// lock opens path so that no other handle can read it, the way a
// database or a mail client holds its files.
func lock(t *testing.T, path string) {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
}

func TestSnapshotReadsLockedFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "locked.db")
	if err := os.WriteFile(file, []byte("data only the snapshot can read"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock(t, file)
	if _, err := os.ReadFile(file); err == nil {
		t.Fatal("the file is not actually locked")
	}

	set, skipped, err := Create([]string{dir}, 5*time.Minute)
	if err != nil {
		// Snapshots need administrator rights. CI runs elevated, so a
		// failure there is real.
		if os.Getenv("CI") == "" && strings.Contains(err.Error(), "access denied") {
			t.Skip("not running as administrator: ", err)
		}
		t.Fatalf("Create: %v (skipped: %v)", err, skipped)
	}
	if len(skipped) != 0 {
		t.Fatalf("volumes skipped: %v", skipped)
	}
	shadow := set.Path(file)
	if shadow == file || !strings.Contains(shadow, "HarddiskVolumeShadowCopy") {
		t.Fatalf("path was not mapped into the snapshot: %s", shadow)
	}
	got, err := os.ReadFile(shadow)
	if err != nil || string(got) != "data only the snapshot can read" {
		t.Fatalf("read from snapshot: %q %v", got, err)
	}
	// The snapshot is frozen: later changes do not show up in it.
	late := filepath.Join(dir, "created-later.txt")
	os.WriteFile(late, []byte("x"), 0o644)
	if _, err := os.Stat(set.Path(late)); err == nil {
		t.Fatal("a file created after the snapshot is visible in it")
	}
	// Listing through the snapshot works, including the volume root.
	if _, err := os.ReadDir(set.Path(dir)); err != nil {
		t.Fatal(err)
	}
	vol, _ := VolumeOf(dir)
	if _, err := os.ReadDir(set.Path(vol)); err != nil {
		t.Fatalf("listing the snapshot root: %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.ReadFile(shadow); err == nil {
		t.Fatal("the snapshot still exists after Close")
	}
}

func TestPathMapping(t *testing.T) {
	s := &Set{mounts: []mount{{`C:\mnt\data\`, `\\?\GLOBALROOT\Device\Shadow2`}, {`C:\`, `\\?\GLOBALROOT\Device\Shadow1`}}}
	for in, want := range map[string]string{
		`C:\`:                 `\\?\GLOBALROOT\Device\Shadow1\`,
		`c:\Users\a.txt`:      `\\?\GLOBALROOT\Device\Shadow1\Users\a.txt`,
		`C:\mnt\data`:         `\\?\GLOBALROOT\Device\Shadow2\`,
		`C:\mnt\data\x\y.txt`: `\\?\GLOBALROOT\Device\Shadow2\x\y.txt`,
		`C:\mnt\database`:     `\\?\GLOBALROOT\Device\Shadow1\mnt\database`,
		`D:\other`:            `D:\other`,
	} {
		if got := s.Path(in); got != want {
			t.Errorf("Path(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWriters reads the metadata of the real writers on this machine.
// Every Windows installation has a registry writer and a system writer.
func TestWriters(t *testing.T) {
	ws, err := Writers(5 * time.Minute)
	if err != nil {
		if os.Getenv("CI") == "" && strings.Contains(err.Error(), "access denied") {
			t.Skip("not running as administrator: ", err)
		}
		t.Fatal(err)
	}
	for _, w := range ws {
		n := 0
		for _, c := range w.Components {
			n += len(c.Files)
		}
		t.Logf("writer %q usage %q: %d components, %d file lists, %d exclusions, system state %v", w.Name, w.Usage, len(w.Components), n, len(w.Excludes), w.IsSystemState())
	}
	files, excludes, names := SystemState(ws)
	t.Logf("system state: %d file lists and %d exclusions from %v", len(files), len(excludes), names)
	if len(files) == 0 {
		t.Fatal("no system state files reported")
	}
	sel := NewSelection(files, excludes)
	hive := filepath.Join(os.Getenv("SystemRoot"), "System32", "config", "SYSTEM")
	if !sel.Match(hive, false) {
		for _, f := range files {
			if strings.Contains(strings.ToLower(f.Path), "config") {
				t.Logf("file list: %+v", f)
			}
		}
		t.Fatalf("the registry hive %s is not part of the system state", hive)
	}
	if sel.Match(filepath.Join(os.Getenv("SystemDrive")+`\`, "pagefile.sys"), false) {
		t.Fatal("the page file is part of the system state")
	}
	for _, f := range files {
		if strings.Contains(f.Path, "%") {
			t.Errorf("unexpanded variable in %+v", f)
		}
	}
	if len(sel.Roots()) == 0 {
		t.Fatal("no drives")
	}
}
