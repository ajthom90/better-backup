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
	ioctlVolumeDiskExtents = 0x00560000
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
	v := &volumeWriter{w: f}
	// Write through the disk under the volume where there is exactly
	// one: Windows may try to mount a half-written file system when
	// something else looks at the volume, and then refuses writes made
	// through the volume itself. The lock held here keeps the disk
	// region writable.
	var disk *os.File
	var ext struct {
		Count  uint32
		_      uint32
		Disk   uint32
		_      uint32
		Start  int64
		Length int64
	}
	if err := windows.DeviceIoControl(h, ioctlVolumeDiskExtents, nil, 0, (*byte)(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), &n, nil); err == nil && ext.Count == 1 && ext.Length >= size {
		dp, _ := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, ext.Disk))
		if dh, err := windows.CreateFile(dp, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0); err == nil {
			disk = os.NewFile(uintptr(dh), "disk")
			v.w, v.base = disk, ext.Start
		}
	}
	// Blank the boot sector so the old file system is not recognized
	// while its replacement is incomplete.
	if _, err := v.write(make([]byte, 4096), v.base); err != nil {
		f.Close()
		if disk != nil {
			disk.Close()
		}
		return nil, nil, fmt.Errorf("writing to %s: %w", dev, err)
	}
	return v, func() error {
		err := v.finish()
		if disk != nil {
			if cerr := disk.Close(); err == nil {
				err = cerr
			}
		}
		// Drop the blank file system Windows may have mounted meanwhile.
		// Closing the handle releases the lock, and Windows mounts the
		// restored file system the next time the volume is used.
		windows.DeviceIoControl(h, fsctlDismountVolume, nil, 0, nil, 0, &n, nil)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}, nil
}

// volumeWriter writes an image to a volume and holds back the first
// block, which contains the boot sector, until everything else is there.
type volumeWriter struct {
	w     io.WriterAt
	base  int64
	first []byte
	buf   []byte
}

func (v *volumeWriter) WriteAt(p []byte, off int64) (int, error) {
	if off == 0 {
		v.first = append([]byte(nil), p...)
		return len(p), nil
	}
	return v.write(p, v.base+off)
}

// write copies p into sector-aligned memory first. Disk devices reject
// buffers at odd addresses with "The parameter is incorrect", and chunks
// from the repository can sit anywhere inside a larger buffer.
func (v *volumeWriter) write(p []byte, off int64) (int, error) {
	const align = 4096
	if len(v.buf) < len(p)+align {
		v.buf = make([]byte, len(p)+align)
	}
	skip := (align - int(uintptr(unsafe.Pointer(&v.buf[0]))%align)) % align
	b := v.buf[skip : skip+len(p)]
	copy(b, p)
	return v.w.WriteAt(b, off)
}

func (v *volumeWriter) finish() error {
	if v.first == nil {
		return nil
	}
	_, err := v.write(v.first, v.base)
	return err
}
