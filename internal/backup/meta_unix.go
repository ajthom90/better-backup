//go:build unix

package backup

import (
	"io/fs"
	"syscall"

	"github.com/ajthom90/better-backup/internal/tree"
)

func fillMeta(e *tree.Entry, fi fs.FileInfo) {
	e.Mode = tree.ModeBits(fi.Mode())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		e.UID, e.GID = uint32(st.Uid), uint32(st.Gid)
	}
}
