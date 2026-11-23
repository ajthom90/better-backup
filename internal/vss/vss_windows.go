//go:build windows

// Package vss takes Volume Shadow Copy snapshots on Windows, so a backup
// reads a frozen, consistent view of each volume. Files that are locked
// or being written can be read from the snapshot, and VSS-aware
// applications flush their data before the snapshot is taken.
package vss

import (
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	vssapi         = windows.NewLazySystemDLL("vssapi.dll")
	procCreate     = vssapi.NewProc("CreateVssBackupComponentsInternal")
	procFreeProps  = vssapi.NewProc("VssFreeSnapshotPropertiesInternal")
	ole32          = windows.NewLazySystemDLL("ole32.dll")
	procCoInitSecu = ole32.NewProc("CoInitializeSecurity")
)

// Method slots in the COM vtables, counted from IUnknown.
const (
	unkRelease = 2

	bcInitializeForBackup   = 5
	bcSetBackupState        = 6
	bcGatherWriterMetadata  = 9
	bcGetWriterMetadataCnt  = 10
	bcGetWriterMetadata     = 11
	bcFreeWriterMetadata    = 12
	bcPrepareForBackup      = 14
	bcAbortBackup           = 15
	bcBackupComplete        = 27
	bcSetContext            = 35
	bcStartSnapshotSet      = 36
	bcAddToSnapshotSet      = 37
	bcDoSnapshotSet         = 38
	bcGetSnapshotProperties = 42

	// IVssExamineWriterMetadata
	wmSaveAsXML = 12

	asyncCancel      = 3
	asyncWait        = 4
	asyncQueryStatus = 5

	ctxBackup  = 0 // VSS_CTX_BACKUP: writers take part, snapshot is released with the requester
	btCopy     = 5 // VSS_BT_COPY: writers do not truncate logs or update backup history
	sFinished  = 0x0004230A
	sPending   = 0x00042309
	rpcTooLate = 0x80010119
)

type snapshotProp struct {
	SnapshotID           windows.GUID
	SnapshotSetID        windows.GUID
	SnapshotsCount       int32
	SnapshotDeviceObject *uint16
	OriginalVolumeName   *uint16
	OriginatingMachine   *uint16
	ServiceMachine       *uint16
	ExposedName          *uint16
	ExposedPath          *uint16
	ProviderID           windows.GUID
	SnapshotAttributes   int32
	CreationTimestamp    int64
	Status               int32
}

// call invokes method slot of the COM object.
func call(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(obj)
	all := append([]uintptr{uintptr(obj)}, args...)
	r, _, _ := syscall.SyscallN(vtbl[slot], all...)
	return r
}

// inProgressError is VSS_E_SNAPSHOT_SET_IN_PROGRESS: retry later.
type inProgressError struct{ error }

// providerError is VSS_E_UNEXPECTED_PROVIDER_ERROR. The provider gives
// it for lasting faults, but also when several snapshots are made at
// once, so it is worth a few more tries.
type providerError struct{ error }

func check(hr uintptr, what string) error {
	if int32(hr) >= 0 {
		return nil
	}
	code := uint32(hr)
	if code == 0x80042316 {
		return inProgressError{fmt.Errorf("VSS %s failed (0x80042316): another snapshot is already being created", what)}
	}
	if code == 0x8004230F {
		return providerError{fmt.Errorf("VSS %s failed (0x8004230F): the shadow copy provider had an unexpected error", what)}
	}
	msg := map[uint32]string{
		0x80070005: "access denied: the agent must run as an administrator or as a service",
		0x8004230C: "the volume does not support snapshots",
		0x8004231F: "not enough disk space for the shadow copy storage",
		0x80042308: "the volume was not found",
	}[code]
	if msg == "" {
		msg = windows.Errno(code).Error()
	}
	return fmt.Errorf("VSS %s failed (0x%08X): %s", what, code, msg)
}

// guidArg passes a GUID by value as the platform's calling convention
// requires: by hidden reference on x64, in two registers on arm64.
func guidArg(g *windows.GUID) []uintptr {
	if runtime.GOARCH == "arm64" {
		w := (*[2]uint64)(unsafe.Pointer(g))
		return []uintptr{uintptr(w[0]), uintptr(w[1])}
	}
	return []uintptr{uintptr(unsafe.Pointer(g))}
}

func wait(async unsafe.Pointer, what string, deadline time.Time) error {
	defer call(async, unkRelease)
	for {
		if err := check(call(async, asyncWait, 500), what); err != nil {
			return err
		}
		var status int32
		if err := check(call(async, asyncQueryStatus, uintptr(unsafe.Pointer(&status)), 0), what); err != nil {
			return err
		}
		switch {
		case uint32(status) == sFinished:
			return nil
		case uint32(status) != sPending && status != 0:
			// A status of 0 has been seen from an operation that had not
			// finished: keep waiting, like for "pending".
			if err := check(uintptr(uint32(status)), what); err != nil {
				return err
			}
			return fmt.Errorf("VSS %s ended with status 0x%08X", what, uint32(status))
		case time.Now().After(deadline):
			call(async, asyncCancel)
			return fmt.Errorf("VSS %s timed out", what)
		}
	}
}

type mount struct {
	volume string // e.g. `C:\`, always ending in a backslash
	device string // e.g. `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy7`
}

// Set is a group of snapshots taken at the same instant, one per volume.
type Set struct {
	mounts []mount
	stop   chan struct{}
	done   chan error
}

// VolumeOf returns the mount point of the volume holding path.
func VolumeOf(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", fmt.Errorf("finding the volume of %s: %w", path, err)
	}
	v := windows.UTF16ToString(buf)
	if !strings.HasSuffix(v, `\`) {
		v += `\`
	}
	return v, nil
}

// Create snapshots the volumes that hold the given paths. Volumes that
// cannot be snapshotted (network shares, some removable media) are
// reported in skipped and their paths map to themselves. It fails if no
// volume could be snapshotted.
//
// The caller must Close the set when the backup is done.
func Create(paths []string, timeout time.Duration) (set *Set, skipped []string, err error) {
	seen := map[string]bool{}
	var volumes []string
	for _, p := range paths {
		v, err := VolumeOf(p)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if k := strings.ToLower(v); !seen[k] {
			seen[k] = true
			volumes = append(volumes, v)
		}
	}
	if len(volumes) == 0 {
		return nil, skipped, errors.New("no volume to snapshot")
	}
	s := &Set{stop: make(chan struct{}), done: make(chan error, 1)}
	ready := make(chan error, 1)
	// COM objects belong to the thread that made them, so one locked
	// thread owns the snapshot from creation to release.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// S_FALSE (1) means COM was already initialized on this thread.
		if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
			ready <- fmt.Errorf("initializing COM: %w", err)
			return
		}
		defer windows.CoUninitialize()
		// Writers call back into the requester; allow that. A process
		// can set this only once, so "too late" is fine.
		hr, _, _ := procCoInitSecu.Call(0, ^uintptr(0), 0, 0, 6 /* PKT_PRIVACY */, 2 /* IDENTIFY */, 0, 0, 0)
		if int32(hr) < 0 && uint32(hr) != rpcTooLate {
			ready <- check(hr, "security setup")
			return
		}
		// Windows allows one snapshot set to be created at a time,
		// system-wide. If another backup tool is making one, wait for it.
		deadline := time.Now().Add(timeout)
		var bc unsafe.Pointer
		var skip []string
		var err error
		for tries := 0; ; {
			bc, skip, err = s.create(volumes, deadline)
			var busy inProgressError
			var prov providerError
			if errors.As(err, &prov) {
				tries++
			}
			if !errors.As(err, &busy) && (!errors.As(err, &prov) || tries > 3) || time.Now().Add(10*time.Second).After(deadline) {
				break
			}
			time.Sleep(2*time.Second + time.Duration(rand.Int63n(int64(3*time.Second))))
		}
		skipped = append(skipped, skip...)
		ready <- err
		if err != nil {
			return
		}
		<-s.stop
		var cerr error
		var async unsafe.Pointer
		if cerr = check(call(bc, bcBackupComplete, uintptr(unsafe.Pointer(&async))), "backup complete"); cerr == nil {
			cerr = wait(async, "backup complete", time.Now().Add(timeout))
		}
		call(bc, unkRelease) // releases the snapshots
		s.done <- cerr
	}()
	if err := <-ready; err != nil {
		return nil, skipped, err
	}
	return s, skipped, nil
}

func (s *Set) create(volumes []string, deadline time.Time) (bc unsafe.Pointer, skipped []string, err error) {
	if err := procCreate.Find(); err != nil {
		return nil, nil, fmt.Errorf("VSS is not available: %w", err)
	}
	hr, _, _ := procCreate.Call(uintptr(unsafe.Pointer(&bc)))
	if err := check(hr, "startup"); err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			call(bc, bcAbortBackup)
			call(bc, unkRelease)
		}
	}()
	var async unsafe.Pointer
	step := func(what string, slot int, args ...uintptr) error {
		return check(call(bc, slot, args...), what)
	}
	asyncStep := func(what string, slot int) error {
		if err := step(what, slot, uintptr(unsafe.Pointer(&async))); err != nil {
			return err
		}
		return wait(async, what, deadline)
	}
	if err = step("initialize", bcInitializeForBackup, 0); err != nil {
		return
	}
	if err = step("set context", bcSetContext, ctxBackup); err != nil {
		return
	}
	if err = step("set backup state", bcSetBackupState, 0, 0, btCopy, 0); err != nil {
		return
	}
	if err = asyncStep("gather writer metadata", bcGatherWriterMetadata); err != nil {
		return
	}
	call(bc, bcFreeWriterMetadata)
	var setID windows.GUID
	if err = step("start snapshot set", bcStartSnapshotSet, uintptr(unsafe.Pointer(&setID))); err != nil {
		return
	}
	ids := map[string]*windows.GUID{}
	for _, v := range volumes {
		name, _ := windows.UTF16PtrFromString(v)
		var provider, id windows.GUID
		args := append([]uintptr{uintptr(unsafe.Pointer(name))}, guidArg(&provider)...)
		args = append(args, uintptr(unsafe.Pointer(&id)))
		if e := step("add volume "+v, bcAddToSnapshotSet, args...); e != nil {
			skipped = append(skipped, e.Error())
			continue
		}
		runtime.KeepAlive(&provider)
		ids[v] = &id
	}
	if len(ids) == 0 {
		err = errors.New("no volume could be added to the snapshot: " + strings.Join(skipped, "; "))
		return
	}
	if err = asyncStep("prepare for backup", bcPrepareForBackup); err != nil {
		return
	}
	if err = asyncStep("create snapshot", bcDoSnapshotSet); err != nil {
		return
	}
	for v, id := range ids {
		var prop snapshotProp
		args := append(guidArg(id), uintptr(unsafe.Pointer(&prop)))
		if err = step("read snapshot properties", bcGetSnapshotProperties, args...); err != nil {
			return
		}
		runtime.KeepAlive(id)
		s.mounts = append(s.mounts, mount{volume: v, device: windows.UTF16PtrToString(prop.SnapshotDeviceObject)})
		procFreeProps.Call(uintptr(unsafe.Pointer(&prop)))
	}
	// Longest mount point first, so volumes mounted in folders win over
	// the volume that holds the folder.
	sort.Slice(s.mounts, func(i, j int) bool { return len(s.mounts[i].volume) > len(s.mounts[j].volume) })
	return bc, skipped, nil
}

// Path maps a path on a live volume to the same path in its snapshot.
// Paths on volumes without a snapshot are returned unchanged.
func (s *Set) Path(p string) string {
	if s == nil {
		return p
	}
	full := p
	if !strings.HasSuffix(full, `\`) {
		full += `\`
	}
	for _, m := range s.mounts {
		if len(full) >= len(m.volume) && strings.EqualFold(full[:len(m.volume)], m.volume) {
			rest := ""
			if len(p) > len(m.volume) {
				rest = p[len(m.volume):]
			}
			return m.device + `\` + rest
		}
	}
	return p
}

// Volumes lists the volumes that were snapshotted.
func (s *Set) Volumes() []string {
	var out []string
	for _, m := range s.mounts {
		out = append(out, m.volume)
	}
	return out
}

// Close tells the writers the backup is complete and releases the
// snapshots.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	close(s.stop)
	return <-s.done
}

// EnablePrivileges turns on the backup and restore privileges the
// process token holds, so the agent can read and write every file and
// its owner and permissions regardless of the file's ACL. It reports the
// privileges that could not be requested; a token that simply lacks one
// (a non-administrator) is not reported, and those files are skipped
// with a warning during the backup instead.
func EnablePrivileges() (missing []string) {
	names := []string{"SeBackupPrivilege", "SeRestorePrivilege", "SeSecurityPrivilege", "SeTakeOwnershipPrivilege"}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return names
	}
	defer token.Close()
	for _, n := range names {
		var luid windows.LUID
		np, _ := windows.UTF16PtrFromString(n)
		if windows.LookupPrivilegeValue(nil, np, &luid) != nil {
			missing = append(missing, n)
			continue
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
			missing = append(missing, n)
		}
	}
	return missing
}
