//go:build windows

package backup

import (
	"io/fs"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/tree"
)

func fillMeta(e *tree.Entry, path string, fi fs.FileInfo) error {
	e.Mode = tree.ModeBits(fi.Mode())
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		e.WinAttrs = d.FileAttributes
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil // the link's target carries the permissions that matter
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	e.WinSD = append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(sd)), sd.Length())...)
	return nil
}
