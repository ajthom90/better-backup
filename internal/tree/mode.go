package tree

import "io/fs"

// ModeBits converts a Go file mode to Unix permission and special bits.
func ModeBits(m fs.FileMode) uint32 {
	b := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		b |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		b |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		b |= 0o1000
	}
	return b
}

// FileMode converts stored bits back to a Go file mode.
func FileMode(b uint32) fs.FileMode {
	m := fs.FileMode(b & 0o777)
	if b&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if b&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if b&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}
