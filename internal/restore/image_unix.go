//go:build unix

package restore

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func isDevice(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeDevice != 0
}

// openImageTarget opens a block device, or creates a file, to receive an
// image.
func openImageTarget(path string, size int64) (io.WriterAt, func() error, error) {
	if !isDevice(path) {
		return createImageFile(path, size)
	}
	if mounted(path) {
		return nil, nil, fmt.Errorf("%s is mounted; unmount it before restoring an image over it", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	if have, err := f.Seek(0, io.SeekEnd); err != nil || have < size {
		f.Close()
		return nil, nil, fmt.Errorf("%s holds %d bytes and the image needs %d", path, have, size)
	}
	return f, func() error {
		err := f.Sync()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}, nil
}

// mounted reports whether a device is in use as a file system (Linux).
func mounted(dev string) bool {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	real, _ := filepath.EvalSymlinks(dev)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if m, err := filepath.EvalSymlinks(fields[0]); err == nil && (m == real || m == dev) {
			return true
		}
	}
	return false
}
