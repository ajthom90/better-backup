//go:build unix

package zfs

import (
	"os"
	"syscall"
)

// sameFS reports whether two paths are on the same mounted file system.
// known is false when that cannot be told.
func sameFS(a, b string) (same, known bool) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, false
	}
	sa, ok1 := fa.Sys().(*syscall.Stat_t)
	sb, ok2 := fb.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, false
	}
	return sa.Dev == sb.Dev, true
}
