package restore

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlDiskGetLengthInfo = 0x0007405C
	fsctlLockVolume        = 0x00090018
	fsctlDismountVolume    = 0x00090020
)

var driveLetter = regexp.MustCompile(`^[A-Za-z]:\\?$`)

// isDevice reports whether path names a volume (X:) or a device path.
func isDevice(path string) bool {
	return driveLetter.MatchString(path) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\\?\`)
}

// openImageTarget opens a volume, or creates a file, to receive an
// image. A volume is locked and dismounted first, which fails while any
// program has files open on it.
func openImageTarget(path string, size int64) (io.WriterAt, func() error, error) {
	if !isDevice(path) {
		return createImageFile(path, size)
	}
	dev := strings.TrimRight(path, `\`)
	if driveLetter.MatchString(path) {
		dev = `\\.\` + path[:2]
	}
	p, err := windows.UTF16PtrFromString(dev)
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", dev, err)
	}
	f := os.NewFile(uintptr(h), dev)
	var n uint32
	var have int64
	if err := windows.DeviceIoControl(h, ioctlDiskGetLengthInfo, nil, 0, (*byte)(unsafe.Pointer(&have)), 8, &n, nil); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("reading the size of %s: %w", dev, err)
	}
	if have < size {
		f.Close()
		return nil, nil, fmt.Errorf("%s holds %d bytes and the image needs %d", dev, have, size)
	}
	// Other programs (search indexing, antivirus) often hold a volume for
	// a moment, so try for a few seconds before giving up.
	err = windows.DeviceIoControl(h, fsctlLockVolume, nil, 0, nil, 0, &n, nil)
	for i := 0; err != nil && i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		err = windows.DeviceIoControl(h, fsctlLockVolume, nil, 0, nil, 0, &n, nil)
	}
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s is in use and cannot be locked (%w); close the programs using it, or for a system volume restore from recovery media", dev, err)
	}
	if err := windows.DeviceIoControl(h, fsctlDismountVolume, nil, 0, nil, 0, &n, nil); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("dismounting %s: %w", dev, err)
	}
	// Closing the handle releases the lock; Windows mounts the restored
	// file system the next time the volume is used.
	return f, f.Close, nil
}
