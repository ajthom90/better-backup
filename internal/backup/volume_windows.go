package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlDiskGetLengthInfo = 0x0007405C
	fsctlGetNTFSVolumeData = 0x00090064
	fsctlGetVolumeBitmap   = 0x0009006F
	fsctlAllowExtendedDASD = 0x00090083
)

// OpenImage opens a volume for an image backup. name is recorded in the
// snapshot; readPath is the device to read, normally the volume's shadow
// copy (\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopyN) or \\.\X:.
//
// On NTFS the allocation bitmap is read so free space is skipped. Other
// file systems are copied in full.
func OpenImage(name, readPath string) (ImageSource, error) {
	p, err := windows.UTF16PtrFromString(readPath)
	if err != nil {
		return ImageSource{}, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ImageSource{}, fmt.Errorf("opening %s: %w", readPath, err)
	}
	f := os.NewFile(uintptr(h), readPath)
	var size int64
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlDiskGetLengthInfo, nil, 0, (*byte)(unsafe.Pointer(&size)), 8, &n, nil); err != nil {
		f.Close()
		return ImageSource{}, fmt.Errorf("reading the size of %s: %w", readPath, err)
	}
	// A mounted file system only lets its own sectors be read through
	// the volume. Ask for the whole partition, which on NTFS adds the
	// backup boot sector behind the last cluster.
	windows.DeviceIoControl(h, fsctlAllowExtendedDASD, nil, 0, nil, 0, &n, nil)
	src := ImageSource{Name: name, Size: size, R: f, Close: func() { f.Close() }}
	if used, fsEnd, err := ntfsUsed(h, size); err == nil {
		src.Used = used
		src.R = &tailReader{f, fsEnd}
	}
	return src, nil
}

// tailReader reads a volume and returns zeros for whatever cannot be
// read behind the end of the file system. Shadow copies do not always
// expose that last piece of the partition; it holds only a spare copy
// of the boot sector.
type tailReader struct {
	r     io.ReaderAt
	fsEnd int64
}

func (t *tailReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := t.r.ReadAt(p, off)
	if n < len(p) && off+int64(n) >= t.fsEnd {
		clear(p[n:])
		return len(p), nil
	}
	return n, err
}

// ntfsUsed returns the allocated ranges of an NTFS volume.
func ntfsUsed(h windows.Handle, size int64) (used []Extent, fsEnd int64, err error) {
	// NTFS_VOLUME_DATA_BUFFER: total clusters at offset 16, bytes per
	// cluster at offset 44.
	var data [96]byte
	var n uint32
	if err := windows.DeviceIoControl(h, fsctlGetNTFSVolumeData, nil, 0, &data[0], uint32(len(data)), &n, nil); err != nil {
		return nil, 0, err
	}
	total := *(*int64)(unsafe.Pointer(&data[16]))
	bpc := int64(*(*uint32)(unsafe.Pointer(&data[44])))
	if total <= 0 || bpc <= 0 || total*bpc > size {
		return nil, 0, errors.New("implausible NTFS volume data")
	}
	add := func(off, length int64) {
		if k := len(used) - 1; k >= 0 && used[k].Off+used[k].Len == off {
			used[k].Len += length
			return
		}
		used = append(used, Extent{off, length})
	}
	// VOLUME_BITMAP_BUFFER: starting LCN, cluster count from there to the
	// end of the volume, then one bit per cluster.
	buf := make([]byte, 16+(1<<20))
	for lcn := int64(0); lcn < total; {
		err := windows.DeviceIoControl(h, fsctlGetVolumeBitmap, (*byte)(unsafe.Pointer(&lcn)), 8, &buf[0], uint32(len(buf)), &n, nil)
		if err != nil && err != windows.ERROR_MORE_DATA {
			return nil, 0, err
		}
		if n <= 16 {
			return nil, 0, errors.New("empty volume bitmap")
		}
		start := *(*int64)(unsafe.Pointer(&buf[0]))
		count := min(*(*int64)(unsafe.Pointer(&buf[8])), int64(n-16)*8)
		bits := buf[16:n]
		for i := int64(0); i < count; i++ {
			if bits[i/8] == 0 && i%8 == 0 && i+8 <= count {
				i += 7
				continue
			}
			if bits[i/8]&(1<<(i%8)) != 0 {
				add((start+i)*bpc, bpc)
			}
		}
		lcn = start + count
		if err == nil {
			break
		}
	}
	// The space after the last cluster holds the backup boot sector.
	if tail := total * bpc; tail < size {
		add(tail, size-tail)
	}
	return used, total * bpc, nil
}
