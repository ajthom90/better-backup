//go:build windows

package restore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	advapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procRegLoad     = advapi32.NewProc("RegLoadKeyW")
	procRegUnload   = advapi32.NewProc("RegUnLoadKeyW")
	procRegSave     = advapi32.NewProc("RegSaveKeyExW")
	procRegReplace  = advapi32.NewProc("RegReplaceKeyW")
	procRegCopyTree = advapi32.NewProc("RegCopyTreeW")
	procRegDelTree  = advapi32.NewProc("RegDeleteTreeW")
	procRegSetValue = advapi32.NewProc("RegSetValueExW")
)

func regErr(r uintptr, what string) error {
	if r == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", what, syscall.Errno(r))
}

func u16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// sysRun is the state of one in-place system state restore.
type sysRun struct {
	opts      SystemState
	configDir string
	stage     string            // where restored hives wait, beside the live ones
	hives     map[string]string // hive name → staged file
	pending   int               // files Windows swaps in at the restart
	dc        bool              // this machine is a domain controller
	ad        bool              // the Active Directory database was restored
	sysvol    bool              // a SYSVOL folder was restored
	warn      func(string, ...any)
}

// adRunning reports whether this machine is a domain controller and
// whether Active Directory is running on it.
func adRunning() (dc, running bool) {
	m, err := mgr.Connect()
	if err != nil {
		return false, false
	}
	defer m.Disconnect()
	s, err := m.OpenService("NTDS")
	if err != nil {
		return false, false
	}
	defer s.Close()
	st, err := s.Query()
	return true, err == nil && st.State != svc.Stopped
}

func (r *restorer) sysBegin() error {
	root := os.Getenv("SystemRoot")
	if root == "" {
		return errors.New("cannot find the Windows folder (SystemRoot is not set)")
	}
	s := &sysRun{opts: *r.opts.SystemState, configDir: filepath.Join(root, "System32", "config"), hives: map[string]string{}, warn: r.warn}
	s.stage = filepath.Join(s.configDir, "bb-restore")
	var running bool
	if s.dc, running = adRunning(); running {
		return errors.New("Active Directory is running on this domain controller, so its database cannot be replaced. " +
			"Restart the machine in Directory Services Restore Mode (run: bcdedit /set safeboot dsrepair, then restart), " +
			"start the agent there and run this restore again")
	}
	if !s.dc && (s.opts.AuthoritativeSYSVOL || len(s.opts.AuthoritativeSubtrees) > 0) {
		return errors.New("an authoritative restore was asked for, but this machine is not a domain controller")
	}
	if s.opts.Registry {
		os.RemoveAll(s.stage) // what it can: hives replaced earlier stay in use until a restart
		if err := os.MkdirAll(s.stage, 0o700); err != nil {
			return err
		}
	}
	r.sys = s
	return nil
}

// place says where a system state file is written: a registry hive goes
// to the staging folder, and files that must not be restored are
// skipped.
func (s *sysRun) place(dest string) (string, bool) {
	switch kind, hive := classify(s.configDir, dest); kind {
	case fileSkip:
		return dest, true
	case fileHive:
		if !s.opts.Registry {
			return dest, true
		}
		s.hives[hive] = filepath.Join(s.stage, hive)
		return s.hives[hive], false
	case fileHiveLog:
		if !s.opts.Registry {
			return dest, true
		}
		return filepath.Join(s.stage, hive), false
	}
	if strings.EqualFold(filepath.Base(dest), "ntds.dit") {
		s.ad = true
	}
	return dest, false
}

// later asks Windows to put tmp in dest's place at the next restart.
func (s *sysRun) later(tmp, dest string) error {
	err := windows.MoveFileEx(u16(tmp), u16(dest), windows.MOVEFILE_DELAY_UNTIL_REBOOT|windows.MOVEFILE_REPLACE_EXISTING)
	if err == nil {
		s.pending++
	}
	return err
}

// afterDir runs when a folder has been restored. In a folder of the
// Active Directory database it removes the engine's logs that the
// backup does not hold: they are newer than the restored database.
func (s *sysRun) afterDir(dir string, names map[string]bool) {
	if names["sysvol"] || strings.EqualFold(filepath.Base(dir), "sysvol") {
		s.sysvol = true
	}
	if !names["ntds.dit"] && !names["edb.log"] && !names["edb.chk"] {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && adLog(e.Name()) && !names[strings.ToLower(e.Name())] {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				s.warn("removing the newer Active Directory log %s: %v", filepath.Join(dir, e.Name()), err)
			}
		}
	}
}

// regValue is a value to write into the SYSTEM hive, under a path that
// starts at the current control set.
type regValue struct {
	key, name string
	str       string // REG_SZ, or
	dword     uint32 // REG_DWORD when isDword
	isDword   bool
}

// marks lists what the restore must tell Windows through the registry.
func (s *sysRun) marks() []regValue {
	id, _ := windows.GenerateGUID()
	v := []regValue{{key: `Control\BackupRestore\SystemStateRestore`, name: "LastRestoreId", str: id.String()}}
	if s.pending > 0 {
		// Lets the restart replace files that Windows protects.
		v = append(v, regValue{key: `Control\Session Manager`, name: "AllowProtectedRenames", dword: 1, isDword: true})
	}
	if s.dc && s.ad {
		// Tells Active Directory its database came from a backup, so it
		// takes a new identity towards its replication partners.
		v = append(v, regValue{key: `Services\NTDS\Parameters`, name: "Database restored from backup", dword: 1, isDword: true})
	}
	if s.dc && (s.sysvol || s.ad) {
		mode, flags := "non-authoritative", uint32(0xD2)
		if s.opts.AuthoritativeSYSVOL {
			mode, flags = "authoritative", 0xD4
		}
		v = append(v, regValue{key: `Services\DFSR\Restore`, name: "SYSVOL", str: mode})
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\NtFrs\Parameters`, registry.QUERY_VALUE); err == nil {
			k.Close()
			v = append(v, regValue{key: `Services\NtFrs\Parameters\Backup/Restore\Process at Startup`, name: "BurFlags", dword: flags, isDword: true})
		}
	}
	return v
}

func setMarks(root registry.Key, set string, marks []regValue) error {
	for _, m := range marks {
		k, _, err := registry.CreateKey(root, set+`\`+m.key, registry.SET_VALUE)
		if err != nil {
			return fmt.Errorf("registry key %s: %w", m.key, err)
		}
		if m.isDword {
			err = k.SetDWordValue(m.name, m.dword)
		} else {
			err = k.SetStringValue(m.name, m.str)
		}
		k.Close()
		if err != nil {
			return fmt.Errorf("registry value %s\\%s: %w", m.key, m.name, err)
		}
	}
	return nil
}

// copyValue makes dst's value the same as src's, absent if it is absent.
func copyValue(src, dst registry.Key, name string) error {
	n, typ, err := src.GetValue(name, nil)
	if err != nil {
		dst.DeleteValue(name)
		return nil
	}
	buf := make([]byte, n+2)
	if n, typ, err = src.GetValue(name, buf); err != nil {
		return err
	}
	var p *byte
	if n > 0 {
		p = &buf[0]
	}
	r, _, _ := procRegSetValue.Call(uintptr(dst), uintptr(unsafe.Pointer(u16(name))), 0, uintptr(typ), uintptr(unsafe.Pointer(p)), uintptr(n))
	return regErr(r, "setting "+name)
}

func copyTree(src registry.Key, dstRoot registry.Key, path string) error {
	procRegDelTree.Call(uintptr(dstRoot), uintptr(unsafe.Pointer(u16(path))))
	dst, _, err := registry.CreateKey(dstRoot, path, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer dst.Close()
	r, _, _ := procRegCopyTree.Call(uintptr(src), 0, uintptr(dst))
	return regErr(r, "copying "+path)
}

// keep carries one KeysNotToRestore entry from the live SYSTEM hive
// into the restored one. set is the restored hive's control set.
func keep(live, restored registry.Key, set, entry string) error {
	kind, key, value, ok := parseKeep(entry)
	if !ok {
		return nil
	}
	src, err := registry.OpenKey(live, key, registry.READ)
	if err != nil {
		return nil // nothing on the live system to keep
	}
	defer src.Close()
	rkey := controlSet(key, set)
	switch kind {
	case keepTree:
		return copyTree(src, restored, rkey)
	case keepValue:
		dst, _, err := registry.CreateKey(restored, rkey, registry.ALL_ACCESS)
		if err != nil {
			return err
		}
		defer dst.Close()
		return copyValue(src, dst, value)
	}
	subs, err := src.ReadSubKeyNames(-1)
	if err != nil {
		return err
	}
	for _, name := range subs {
		lk, err := registry.OpenKey(src, name, registry.READ)
		if err != nil {
			continue
		}
		rk, err := registry.OpenKey(restored, rkey+`\`+name, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			// Only the live system has it: a driver or service installed
			// since the backup.
			err = copyTree(lk, restored, rkey+`\`+name)
			lk.Close()
			if err != nil {
				return err
			}
			continue
		}
		// Both have it: the restored one stands, but never starts later
		// than the live one did.
		if ls, _, err := lk.GetIntegerValue("Start"); err == nil {
			if rs, _, err := rk.GetIntegerValue("Start"); err != nil || ls < rs {
				rk.SetDWordValue("Start", uint32(ls))
			}
		}
		rk.Close()
		lk.Close()
	}
	return nil
}

// patchSystem prepares a restored SYSTEM hive, loaded at restored, to
// take the live one's place: it keeps what the live registry says must
// not be restored, the list of files to swap in at the restart, and the
// restore's own marks.
func patchSystem(live, restored registry.Key, marks []regValue) error {
	sel, err := registry.OpenKey(restored, "Select", registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("the restored SYSTEM hive has no Select key: %w", err)
	}
	cur, _, err := sel.GetIntegerValue("Current")
	sel.Close()
	if err != nil {
		return fmt.Errorf("the restored SYSTEM hive does not name its control set: %w", err)
	}
	set := fmt.Sprintf("ControlSet%03d", cur)
	entries := []string{
		`CurrentControlSet\Control\Session Manager\PendingFileRenameOperations`,
		`CurrentControlSet\Control\Session Manager\PendingFileRenameOperations2`,
	}
	for _, from := range []struct {
		root registry.Key
		path string
	}{{live, `CurrentControlSet\Control\BackupRestore\KeysNotToRestore`}, {restored, set + `\Control\BackupRestore\KeysNotToRestore`}} {
		k, err := registry.OpenKey(from.root, from.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		names, _ := k.ReadValueNames(-1)
		for _, n := range names {
			if list, _, err := k.GetStringsValue(n); err == nil {
				entries = append(entries, list...)
			}
		}
		k.Close()
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if l := strings.ToLower(strings.TrimSpace(e)); l == "" || seen[l] {
			continue
		} else {
			seen[l] = true
		}
		if err := keep(live, restored, set, e); err != nil {
			return fmt.Errorf("keeping %s: %w", e, err)
		}
	}
	return setMarks(restored, set, marks)
}

// hive is one registry hive to replace.
type hive struct {
	root  registry.Key // HKEY_LOCAL_MACHINE or HKEY_USERS
	sub   string       // the key the hive is mounted at
	file  string       // the restored hive file
	patch func(restored registry.Key) error
	ready string // the cleaned hive, written by prepare
}

// prepare loads the restored file, patches it and writes it out again
// as a clean hive without pending log entries.
func (h *hive) prepare() error {
	mount := "bb-restore-" + filepath.Base(h.file)
	procRegUnload.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(u16(mount)))) // left by an interrupted restore
	r, _, _ := procRegLoad.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(u16(mount))), uintptr(unsafe.Pointer(u16(h.file))))
	if err := regErr(r, "loading the restored hive "+h.file); err != nil {
		return err
	}
	defer procRegUnload.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(u16(mount))))
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, mount, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer k.Close()
	if h.patch != nil {
		if err := h.patch(k); err != nil {
			return err
		}
	}
	h.ready = h.file + ".new"
	os.Remove(h.ready)
	const latestFormat = 2 // REG_LATEST_FORMAT
	r, _, _ = procRegSave.Call(uintptr(k), uintptr(unsafe.Pointer(u16(h.ready))), 0, latestFormat)
	return regErr(r, "writing the hive "+h.ready)
}

// replace makes the prepared file the hive's file. For a hive in use
// the change shows at the next restart.
func (h *hive) replace() error {
	// The old file stays in use until the restart, so each run needs a
	// name of its own.
	old := fmt.Sprintf("%s.%d.old", h.file, time.Now().UnixNano())
	r, _, _ := procRegReplace.Call(uintptr(h.root), uintptr(unsafe.Pointer(u16(h.sub))), uintptr(unsafe.Pointer(u16(h.ready))), uintptr(unsafe.Pointer(u16(old))))
	return regErr(r, "replacing the registry hive "+h.sub)
}

func (r *restorer) sysFinish() error {
	s := r.sys
	r.sys = nil
	if s.dc && s.ad {
		for _, dn := range s.opts.AuthoritativeSubtrees {
			if dn = strings.TrimSpace(dn); dn == "" {
				continue
			}
			out, err := exec.CommandContext(r.ctx, "ntdsutil.exe", "popups off", "activate instance ntds", "authoritative restore", "restore subtree "+dn, "quit", "quit").CombinedOutput()
			if err != nil || !strings.Contains(strings.ToLower(string(out)), "successfully") {
				r.warn("marking %s as authoritative failed; run ntdsutil \"authoritative restore\" by hand before restarting: %v %s", dn, err, lastLines(string(out), 3))
			}
		}
	} else if len(s.opts.AuthoritativeSubtrees) > 0 {
		r.warn("no Active Directory database was restored, so nothing was marked as authoritative")
	}
	marks := s.marks()
	if err := setMarks(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet`, marks); err != nil {
		r.warn("system state: %v", err)
	}
	if s.opts.Registry {
		if err := s.registry(marks); err != nil {
			r.warn("the registry was not restored, the rest of the system state was: %v", err)
		}
	}
	if !s.opts.Restart {
		r.warn("restart Windows to finish the restore: %d files in use are swapped in at the restart%s", s.pending, s.dsrmHint())
		return nil
	}
	// Leave Directory Services Restore Mode, if that is where we are.
	exec.Command("bcdedit.exe", "/deletevalue", "safeboot").Run()
	if err := exec.Command("shutdown.exe", "/r", "/t", "60", "/c", "better-backup restored the system state and is restarting Windows").Run(); err != nil {
		r.warn("could not restart Windows; restart it by hand to finish the restore: %v", err)
	}
	return nil
}

func (s *sysRun) dsrmHint() string {
	if !s.dc {
		return ""
	}
	return ". To leave Directory Services Restore Mode run: bcdedit /deletevalue safeboot"
}

// registry replaces the machine hives with the restored ones. All are
// prepared before any is replaced, so a failure leaves the registry
// whole.
func (s *sysRun) registry(marks []regValue) error {
	if _, ok := s.hives["SYSTEM"]; !ok {
		return errors.New("the backup holds no SYSTEM hive")
	}
	live, err := registry.OpenKey(registry.LOCAL_MACHINE, "SYSTEM", registry.READ)
	if err != nil {
		return err
	}
	defer live.Close()
	var hs []*hive
	for _, name := range machineHives {
		file, ok := s.hives[name]
		if !ok {
			continue
		}
		h := &hive{root: registry.LOCAL_MACHINE, sub: name, file: file}
		switch name {
		case "DEFAULT":
			h.root, h.sub = registry.USERS, ".DEFAULT"
		case "SYSTEM":
			h.patch = func(k registry.Key) error { return patchSystem(live, k, marks) }
		}
		if err := h.prepare(); err != nil {
			return err
		}
		hs = append(hs, h)
	}
	for i, h := range hs {
		if err := h.replace(); err != nil {
			if i > 0 {
				return fmt.Errorf("%w; %d hives were already replaced, so restore the registry again before restarting", err, i)
			}
			return err
		}
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r", "")), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}
