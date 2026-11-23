//go:build windows

package restore

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/tree"
)

func (r *restorer) chown(string, *tree.Entry) {}

// platformMeta restores the owner, group and permissions, then the
// read-only, hidden, system and archive bits.
func (r *restorer) platformMeta(path string, e *tree.Entry) {
	if r.opts.SourceOS != "windows" {
		return
	}
	if err := setSecurity(path, e.WinSD); err != nil {
		r.warn("%s: restoring permissions: %v", path, err)
	}
	const mask = syscall.FILE_ATTRIBUTE_READONLY | syscall.FILE_ATTRIBUTE_HIDDEN | syscall.FILE_ATTRIBUTE_SYSTEM | syscall.FILE_ATTRIBUTE_ARCHIVE
	if attrs := e.WinAttrs & mask; attrs != 0 {
		if p, err := syscall.UTF16PtrFromString(path); err == nil {
			if err := syscall.SetFileAttributes(p, attrs); err != nil {
				r.warn("%s: %v", path, err)
			}
		}
	}
}

func setSecurity(path string, raw []byte) error {
	// The smallest valid self-relative descriptor is its 20-byte header.
	if len(raw) < 20 {
		return nil
	}
	buf := append([]byte(nil), raw...)
	sd := (*windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(&buf[0]))
	if !sd.IsValid() || int(sd.Length()) > len(buf) {
		return windows.ERROR_INVALID_SECURITY_DESCR
	}
	owner, _, _ := sd.Owner()
	group, _, _ := sd.Group()
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	// Keep the file's choice of inheriting from its folder or not. When
	// it inherits, Windows recomputes the inherited entries from the
	// folder it is restored into.
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return err
	}
	// Setting another account as owner needs the restore privilege, so
	// it is done separately: without it the permissions still apply.
	if owner != nil {
		info = windows.OWNER_SECURITY_INFORMATION
		if group != nil {
			info |= windows.GROUP_SECURITY_INFORMATION
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, group, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
