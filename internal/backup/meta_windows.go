//go:build windows

package backup

import (
	"errors"
	"io/fs"
	"strings"
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

var (
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procFindFirstStream = kernel32.NewProc("FindFirstStreamW")
	procFindNextStream  = kernel32.NewProc("FindNextStreamW")
)

type findStreamData struct {
	Size int64
	Name [windows.MAX_PATH + 36]uint16
}

// listStreams returns the names of a file's or directory's alternate
// data streams, without the unnamed main stream.
func listStreams(path string) ([]string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var d findStreamData
	h, _, e := procFindFirstStream.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&d)), 0)
	if windows.Handle(h) == windows.InvalidHandle {
		// Directories without streams, and file systems without stream
		// support, report "end of file" or "invalid parameter".
		if errors.Is(e, windows.ERROR_HANDLE_EOF) || errors.Is(e, windows.ERROR_INVALID_PARAMETER) || errors.Is(e, windows.ERROR_INVALID_FUNCTION) {
			return nil, nil
		}
		return nil, e
	}
	defer windows.FindClose(windows.Handle(h))
	var names []string
	for {
		// Names look like ":name:$DATA"; the main stream is "::$DATA".
		full := windows.UTF16ToString(d.Name[:])
		if name := strings.TrimSuffix(strings.TrimPrefix(full, ":"), ":$DATA"); name != "" && strings.HasSuffix(full, ":$DATA") {
			names = append(names, name)
		}
		ok, _, e := procFindNextStream.Call(h, uintptr(unsafe.Pointer(&d)))
		if ok == 0 {
			if errors.Is(e, windows.ERROR_HANDLE_EOF) {
				return names, nil
			}
			return names, e
		}
	}
}
