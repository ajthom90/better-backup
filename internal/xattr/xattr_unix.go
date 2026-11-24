//go:build linux || darwin

package xattr

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Supported says whether this system has extended attributes that this
// package reads.
const Supported = true

// absent reports errors that mean there is nothing to read: no such
// attribute, or a file system without them.
func absent(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM)
}

func get(path, name string) ([]byte, error) {
	for size := 1024; ; {
		buf := make([]byte, size)
		n, err := unix.Lgetxattr(path, name, buf)
		if err == unix.ERANGE {
			if n, err = unix.Lgetxattr(path, name, nil); err != nil {
				return nil, err
			}
			size = n + 1024
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

// Read returns the extended attributes of path itself, not of what a
// link points at. skipped names attributes left out because together
// they exceed MaxSize.
func Read(path string) (attrs map[string][]byte, skipped []string, err error) {
	var names []string
	for size := 1024; ; {
		buf := make([]byte, size)
		n, lerr := unix.Llistxattr(path, buf)
		if lerr == unix.ERANGE {
			if n, lerr = unix.Llistxattr(path, nil); lerr == nil {
				size = n + 1024
				continue
			}
		}
		if lerr != nil {
			if absent(lerr) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("listing extended attributes: %w", lerr)
		}
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name != "" {
				names = append(names, name)
			}
		}
		break
	}
	if probeNFS4 {
		listed := false
		for _, n := range names {
			listed = listed || n == nfs4ACL
		}
		if !listed {
			names = append(names, nfs4ACL)
		}
	}
	sort.Strings(names)
	total := 0
	for _, name := range names {
		v, gerr := get(path, name)
		if gerr != nil {
			if !absent(gerr) && err == nil {
				err = fmt.Errorf("reading extended attribute %s: %w", name, gerr)
			}
			continue
		}
		if total+len(name)+len(v) > MaxSize {
			skipped = append(skipped, name)
			continue
		}
		total += len(name) + len(v)
		if attrs == nil {
			attrs = map[string][]byte{}
		}
		attrs[name] = v
	}
	return attrs, skipped, err
}

// Write sets the given attributes on path. It returns the names it
// could not set, with the first error.
func Write(path string, attrs map[string][]byte) (failed []string, err error) {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if serr := unix.Lsetxattr(path, name, attrs[name], 0); serr != nil {
			failed = append(failed, name)
			if err == nil {
				err = serr
			}
		}
	}
	return failed, err
}
