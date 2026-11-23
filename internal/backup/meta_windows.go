//go:build windows

package backup

import (
	"io/fs"
	"syscall"

	"github.com/ajthom90/better-backup/internal/tree"
)

func fillMeta(e *tree.Entry, fi fs.FileInfo) {
	e.Mode = tree.ModeBits(fi.Mode())
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		e.WinAttrs = d.FileAttributes
	}
}
