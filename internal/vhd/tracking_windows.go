package vhd

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Change tracking ("Resilient Change Tracking") is a record a VHDX keeps
// of which parts of it were written. A tracking id names a moment; the
// disk can later list everything written since.

var (
	procSetInfo = virtdisk.NewProc("SetVirtualDiskInformation")
	procChanges = virtdisk.NewProc("QueryChangesVirtualDisk")
)

const (
	getInfoChangeTracking = 15 // GET_VIRTUAL_DISK_INFO_CHANGE_TRACKING_STATE
	setInfoChangeTracking = 6  // SET_VIRTUAL_DISK_INFO_CHANGE_TRACKING_STATE
)

// Range is a run of bytes of a disk's contents.
type Range struct{ Off, Len int64 }

// openHandle opens a virtual disk file without attaching it.
func openHandle(path string, readOnly bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// OPEN_VIRTUAL_DISK_PARAMETERS, version 2.
	params := struct {
		Version     uint32
		GetInfoOnly int32
		ReadOnly    int32
		Resiliency  windows.GUID
	}{Version: 2}
	if readOnly {
		params.ReadOnly = 1
	}
	var st storageType
	var h windows.Handle
	r, _, _ := procOpen.Call(uintptr(unsafe.Pointer(&st)), uintptr(unsafe.Pointer(p)), 0, 0, uintptr(unsafe.Pointer(&params)), uintptr(unsafe.Pointer(&h)))
	if err := status(r, "opening virtual disk "+path); err != nil {
		return 0, err
	}
	return h, nil
}

// Tracking returns the disk's newest tracking id. The id is empty if
// the disk does not track changes.
func Tracking(path string) (string, error) {
	h, err := openHandle(path, true)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	// GET_VIRTUAL_DISK_INFO: an 8-byte header, then BOOL Enabled, BOOL
	// NewerChanges and the id as UTF-16.
	info := make([]uint16, 1024)
	b := unsafe.Pointer(&info[0])
	*(*uint32)(b) = getInfoChangeTracking
	n := uint32(len(info) * 2)
	r, _, _ := procInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&n)), uintptr(b), 0)
	if err := status(r, "reading the change tracking state of "+path); err != nil {
		return "", err
	}
	if *(*int32)(unsafe.Add(b, 8)) == 0 {
		return "", nil
	}
	return windows.UTF16ToString(info[8:]), nil
}

// SetTracking switches change tracking on or off for a disk that is not
// in use. Hyper-V does this itself for the disks of its machines.
func SetTracking(path string, on bool) error {
	h, err := openHandle(path, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	// SET_VIRTUAL_DISK_INFO: an 8-byte header, then the value.
	var info [32]byte
	*(*uint32)(unsafe.Pointer(&info[0])) = setInfoChangeTracking
	if on {
		*(*int32)(unsafe.Pointer(&info[8])) = 1
	}
	r, _, _ := procSetInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info[0])))
	return status(r, "setting change tracking on "+path)
}

// Changes lists the parts of the disk written since the moment the
// tracking id names, in ascending order. It fails if the disk does not
// know the id; the caller must then read the whole disk.
func Changes(path, since string) ([]Range, error) {
	if since == "" {
		return nil, fmt.Errorf("no change tracking id for %s", path)
	}
	id, err := windows.UTF16PtrFromString(since)
	if err != nil {
		return nil, err
	}
	d, err := Open(path, true)
	if err != nil {
		return nil, err
	}
	size := d.Size()
	d.Close()
	h, err := openHandle(path, true)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	// QUERY_CHANGES_VIRTUAL_DISK_RANGE
	type qrange struct{ Off, Len, Reserved uint64 }
	buf := make([]qrange, 4096)
	out := []Range{}
	for off := int64(0); off < size; {
		count := uint32(len(buf))
		var processed uint64
		r, _, _ := procChanges.Call(uintptr(h), uintptr(unsafe.Pointer(id)), uintptr(off), uintptr(size-off), 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&processed)))
		if err := status(r, "listing the changes of "+path); err != nil {
			return nil, err
		}
		for _, q := range buf[:count] {
			if q.Off >= uint64(size) || q.Len > uint64(size)-q.Off {
				return nil, fmt.Errorf("listing the changes of %s: a range lies outside the disk", path)
			}
			if n := len(out); n > 0 && out[n-1].Off+out[n-1].Len >= int64(q.Off) {
				if end := int64(q.Off + q.Len); end > out[n-1].Off+out[n-1].Len {
					out[n-1].Len = end - out[n-1].Off
				}
				continue
			}
			out = append(out, Range{int64(q.Off), int64(q.Len)})
		}
		if processed == 0 {
			return nil, fmt.Errorf("listing the changes of %s: no progress at offset %d", path, off)
		}
		off += int64(processed)
	}
	return out, nil
}
