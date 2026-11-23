//go:build unix

package backup

import (
	"io/fs"
	"syscall"

	"github.com/ajthom90/better-backup/internal/tree"
)

func fillMeta(e *tree.Entry, _ string, fi fs.FileInfo) error {
	e.Mode = tree.ModeBits(fi.Mode())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		e.UID, e.GID = uint32(st.Uid), uint32(st.Gid)
	}
	return nil
}

// listStreams returns nothing: alternate data streams are an NTFS feature.
func listStreams(string) ([]string, error) { return nil, nil }
