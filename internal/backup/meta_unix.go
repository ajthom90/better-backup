//go:build unix

package backup

import (
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"github.com/ajthom90/better-backup/internal/tree"
	"github.com/ajthom90/better-backup/internal/xattr"
)

func fillMeta(e *tree.Entry, path string, fi fs.FileInfo) error {
	e.Mode = tree.ModeBits(fi.Mode())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		e.UID, e.GID = uint32(st.Uid), uint32(st.Gid)
	}
	attrs, skipped, err := xattr.Read(path)
	e.Xattrs = attrs
	if err == nil && len(skipped) > 0 {
		err = fmt.Errorf("its extended attributes are too large to keep; left out: %s", strings.Join(skipped, ", "))
	}
	return err
}

// listStreams returns nothing: alternate data streams are an NTFS feature.
func listStreams(string) ([]string, error) { return nil, nil }
