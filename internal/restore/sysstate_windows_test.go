//go:build windows

package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/ajthom90/better-backup/internal/vss"
)

func mustKey(t *testing.T, root registry.Key, path string) registry.Key {
	t.Helper()
	k, _, err := registry.CreateKey(root, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

// TestPatchSystem builds a small "live" and a small "restored" SYSTEM
// tree and checks what the restored one keeps from the live one.
func TestPatchSystem(t *testing.T) {
	const base = `Software\better-backup-test`
	procRegDelTree.Call(uintptr(registry.CURRENT_USER), uintptr(unsafe.Pointer(u16(base))))
	t.Cleanup(func() { procRegDelTree.Call(uintptr(registry.CURRENT_USER), uintptr(unsafe.Pointer(u16(base)))) })
	live := mustKey(t, registry.CURRENT_USER, base+`\live`)
	rest := mustKey(t, registry.CURRENT_USER, base+`\restored`)

	// The live system runs on control set 1; the backup was on set 2.
	// (A real hive links CurrentControlSet to it; here it is a plain key.)
	mustKey(t, rest, `Select`).SetDWordValue("Current", 2)
	lc := mustKey(t, live, `CurrentControlSet\Control`)
	mustKey(t, lc, `BackupRestore\KeysNotToRestore`).SetStringsValue("Mount Manager", []string{`MountedDevices\`})
	mustKey(t, rest, `ControlSet002\Control\BackupRestore\KeysNotToRestore`).SetStringsValue("Services", []string{`CurrentControlSet\Services\*`, `MountedDevices\`})
	mustKey(t, lc, `Session Manager`).SetStringsValue("PendingFileRenameOperations", []string{`\??\C:\a.tmp`, `!\??\C:\a`})
	mustKey(t, rest, `ControlSet002\Control\Session Manager`).SetStringValue("Other", "from backup")

	mustKey(t, live, `MountedDevices`).SetBinaryValue(`\DosDevices\C:`, []byte{1, 2, 3})
	mustKey(t, live, `MountedDevices\sub`).SetStringValue("x", "live")
	mustKey(t, rest, `MountedDevices`).SetBinaryValue(`\DosDevices\Z:`, []byte{9})

	ls := mustKey(t, live, `CurrentControlSet\Services`)
	rs := mustKey(t, rest, `ControlSet002\Services`)
	mustKey(t, ls, `newdriver`).SetDWordValue("Start", 0)
	mustKey(t, ls, `newdriver\Parameters`).SetStringValue("p", "v")
	mustKey(t, ls, `both`).SetDWordValue("Start", 1)
	mustKey(t, rs, `both`).SetDWordValue("Start", 3)
	mustKey(t, rs, `both`).SetStringValue("ImagePath", "from backup")
	mustKey(t, ls, `later`).SetDWordValue("Start", 4)
	mustKey(t, rs, `later`).SetDWordValue("Start", 2)
	mustKey(t, rs, `onlybackup`).SetDWordValue("Start", 2)

	marks := []regValue{{key: `Control\BackupRestore\SystemStateRestore`, name: "LastRestoreId", str: "id-1"},
		{key: `Services\NTDS\Parameters`, name: "Database restored from backup", dword: 1, isDword: true}}
	if err := patchSystem(live, rest, marks); err != nil {
		t.Fatal(err)
	}

	md := mustKey(t, rest, `MountedDevices`)
	if b, _, err := md.GetBinaryValue(`\DosDevices\C:`); err != nil || len(b) != 3 {
		t.Errorf("MountedDevices was not taken from the live system: %v %v", b, err)
	}
	if _, _, err := md.GetBinaryValue(`\DosDevices\Z:`); err == nil {
		t.Error("MountedDevices kept a value from the backup")
	}
	if v, _, _ := mustKey(t, rest, `MountedDevices\sub`).GetStringValue("x"); v != "live" {
		t.Error("MountedDevices lost a subkey of the live system")
	}
	sm := mustKey(t, rest, `ControlSet002\Control\Session Manager`)
	if v, _, err := sm.GetStringsValue("PendingFileRenameOperations"); err != nil || len(v) != 2 {
		t.Errorf("pending renames: %v %v", v, err)
	}
	if v, _, _ := sm.GetStringValue("Other"); v != "from backup" {
		t.Error("a value beside a kept one was lost")
	}
	if v, _, _ := mustKey(t, rs, `newdriver\Parameters`).GetStringValue("p"); v != "v" {
		t.Error("a service only the live system has was not carried over")
	}
	for name, want := range map[string]uint64{"both": 1, "later": 2, "onlybackup": 2, "newdriver": 0} {
		if v, _, err := mustKey(t, rs, name).GetIntegerValue("Start"); err != nil || v != want {
			t.Errorf("service %s starts at %d, want %d (%v)", name, v, want, err)
		}
	}
	if v, _, _ := mustKey(t, rs, `both`).GetStringValue("ImagePath"); v != "from backup" {
		t.Error("a service in both lost the backup's settings")
	}
	if v, _, _ := mustKey(t, rest, `ControlSet002\Control\BackupRestore\SystemStateRestore`).GetStringValue("LastRestoreId"); v != "id-1" {
		t.Error("LastRestoreId was not set in the restored hive")
	}
	if v, _, _ := mustKey(t, rest, `ControlSet002\Services\NTDS\Parameters`).GetIntegerValue("Database restored from backup"); v != 1 {
		t.Error("the Active Directory mark was not set in the restored hive")
	}
}

// TestHiveReplace swaps the file under a mounted hive for a restored
// one, the way the machine's own hives are swapped.
func TestHiveReplace(t *testing.T) {
	if missing := vss.EnablePrivileges(); len(missing) > 0 {
		if os.Getenv("CI") == "" {
			t.Skip("not running as administrator: ", missing)
		}
		t.Fatal(missing)
	}
	// Not t.TempDir: Windows keeps the hive files open until a restart.
	dir, err := os.MkdirTemp("", "bb-hive-test-")
	if err != nil {
		t.Fatal(err)
	}
	save := func(name, value string) string {
		const base = `Software\better-backup-hive`
		procRegDelTree.Call(uintptr(registry.CURRENT_USER), uintptr(unsafe.Pointer(u16(base))))
		k := mustKey(t, registry.CURRENT_USER, base)
		k.SetStringValue("v", value)
		file := filepath.Join(dir, name)
		if r, _, _ := procRegSave.Call(uintptr(k), uintptr(unsafe.Pointer(u16(file))), 0, 2); r != 0 {
			t.Fatal(windows.Errno(r))
		}
		procRegDelTree.Call(uintptr(registry.CURRENT_USER), uintptr(unsafe.Pointer(u16(base))))
		return file
	}
	const mount = "better-backup-test-hive"
	load := func(file string) {
		if r, _, _ := procRegLoad.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(u16(mount))), uintptr(unsafe.Pointer(u16(file)))); r != 0 {
			t.Fatal(windows.Errno(r))
		}
	}
	unload := func() {
		procRegUnload.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(u16(mount))))
	}
	read := func(mount string) string {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, mount, registry.QUERY_VALUE)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Close()
		v, _, _ := k.GetStringValue("v")
		p, _, _ := k.GetStringValue("patched")
		return v + "/" + p
	}
	liveFile := save("LIVE", "live")
	restored := save("RESTORED", "from backup, with a longer value so the two files differ in size: "+strings.Repeat("x", 20000))
	load(liveFile)
	t.Cleanup(unload)

	h := &hive{root: registry.LOCAL_MACHINE, sub: mount, file: restored, patch: func(k registry.Key) error { return k.SetStringValue("patched", "yes") }}
	if err := h.prepare(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(h.ready)
	if err != nil {
		t.Fatal(err)
	}
	size := fi.Size() + 1 // never equal to the live file's by accident
	if lf, _ := os.Stat(liveFile); lf != nil && lf.Size() != fi.Size() {
		size = fi.Size()
	}
	if err := h.replace(); err != nil {
		t.Fatal(err)
	}
	// Until the hive is mounted afresh, which for a machine hive means
	// a restart, the old contents stay.
	if got := read(mount); got != "live/" {
		t.Fatalf("while mounted: %q", got)
	}
	// The files have changed places: the prepared one now carries the
	// hive's name and the one in use was moved aside. Windows holds on
	// to both until the restart, so they cannot be read back here.
	if _, err := os.Stat(h.ready); err == nil {
		t.Error("the prepared hive is still under its own name")
	}
	if fi, err := os.Stat(liveFile); err != nil || fi.Size() != size {
		t.Errorf("the file under the hive's name: %v %v, want %d bytes", fi, err, size)
	}
	if olds, _ := filepath.Glob(restored + ".*.old"); len(olds) != 1 {
		t.Errorf("hives moved aside: %v", olds)
	}
	r, _, _ := procRegUnload.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(u16(mount))))
	t.Logf("unloading the replaced hive: %v", windows.Errno(r))
}

// TestLater schedules a file swap for the next restart.
func TestLater(t *testing.T) {
	dir := t.TempDir()
	tmp, dest := filepath.Join(dir, "new.tmp"), filepath.Join(dir, "in-use.dll")
	os.WriteFile(tmp, []byte("new"), 0o600)
	os.WriteFile(dest, []byte("old"), 0o600)
	s := &sysRun{}
	if err := s.later(tmp, dest); err != nil {
		if os.Getenv("CI") == "" {
			t.Skip("not running as administrator: ", err)
		}
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	ops, _, err := k.GetStringsValue("PendingFileRenameOperations")
	found := false
	for i := 0; i+1 < len(ops); i++ {
		// Newer Windows versions put a marker such as *1 in front.
		// The registry holds the long form of a path whose temporary
		// folder was given in the short form.
		if strings.HasSuffix(ops[i], `\new.tmp`) && strings.Contains(ops[i], filepath.Base(dir)) && strings.HasSuffix(ops[i+1], `\in-use.dll`) && strings.Contains(ops[i+1], `!\??\`) {
			found = true
		}
	}
	if err != nil || !found || s.pending != 1 {
		t.Fatalf("pending renames %q, %v, counted %d", ops, err, s.pending)
	}
	if m := s.marks(); len(m) != 2 || m[1].name != "AllowProtectedRenames" {
		t.Fatalf("marks: %+v", m)
	}
}
