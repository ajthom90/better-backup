//go:build windows

package restore

import (
	"syscall"

	"github.com/ajthom90/better-backup/internal/tree"
)

func (r *restorer) chown(string, *tree.Entry) {}

// platformMeta restores the read-only, hidden, system and archive bits.
func (r *restorer) platformMeta(path string, e *tree.Entry) {
	const mask = syscall.FILE_ATTRIBUTE_READONLY | syscall.FILE_ATTRIBUTE_HIDDEN | syscall.FILE_ATTRIBUTE_SYSTEM | syscall.FILE_ATTRIBUTE_ARCHIVE
	attrs := e.WinAttrs & mask
	if r.opts.SourceOS != "windows" || attrs == 0 {
		return
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	if err := syscall.SetFileAttributes(p, attrs); err != nil {
		r.warn("%s: %v", path, err)
	}
}
