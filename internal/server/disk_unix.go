//go:build !windows

package server

import "golang.org/x/sys/unix"

// diskSpace reports the size of the file system holding path and how
// much of it an ordinary user may still fill.
func diskSpace(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), nil
}
