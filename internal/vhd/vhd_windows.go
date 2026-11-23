// Package vhd reads and writes the contents of virtual hard disks (VHDX
// and VHD files) through the Windows virtual disk service. It sees the
// disk the way a virtual machine does: a run of sectors, whatever the
// layout of the file that holds them.
package vhd

import (
	"fmt"
	"io"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	virtdisk   = windows.NewLazySystemDLL("virtdisk.dll")
	procOpen   = virtdisk.NewProc("OpenVirtualDisk")
	procCreate = virtdisk.NewProc("CreateVirtualDisk")
	procAttach = virtdisk.NewProc("AttachVirtualDisk")
	procInfo   = virtdisk.NewProc("GetVirtualDiskInformation")
)

// storageType is VIRTUAL_STORAGE_TYPE. Device id 0 lets Windows work
// the format out from the file.
type storageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

const deviceVHDX = 3

var vendorMicrosoft = windows.GUID{Data1: 0xEC984AEC, Data2: 0xA0F9, Data3: 0x47E9, Data4: [8]byte{0x90, 0x1F, 0x71, 0x41, 0x5A, 0x66, 0x34, 0x5B}}

const (
	attachReadOnly      = 0x1
	attachNoDriveLetter = 0x2
	attachNoLocalHost   = 0x8 // not shown to Windows as a disk; raw reads and writes on the handle instead
)

// Disk is an open virtual disk.
type Disk struct {
	h    windows.Handle
	size int64
	ev   windows.Handle
	buf  []byte
}

func status(r uintptr, what string) error {
	if r == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", what, windows.Errno(r))
}

// Open opens a virtual disk file for raw access to its contents.
func Open(path string, readOnly bool) (*Disk, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	return attach(h, path, readOnly)
}

// Create makes a new dynamically expanding VHDX of the given size and
// opens it for writing. It fails if the file exists.
func Create(path string, size int64) (*Disk, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	if size <= 0 || size%512 != 0 {
		return nil, fmt.Errorf("a virtual disk's size must be a multiple of 512 bytes, got %d", size)
	}
	// CREATE_VIRTUAL_DISK_PARAMETERS, version 2.
	params := struct {
		Version            uint32
		UniqueID           windows.GUID
		MaximumSize        uint64
		BlockSize          uint32
		SectorSize         uint32
		PhysicalSectorSize uint32
		ParentPath         *uint16
		SourcePath         *uint16
		OpenFlags          uint32
		ParentType         storageType
		SourceType         storageType
		Resiliency         windows.GUID
	}{Version: 2, MaximumSize: uint64(size)}
	if params.UniqueID, err = windows.GenerateGUID(); err != nil {
		return nil, err
	}
	st := storageType{DeviceID: deviceVHDX, VendorID: vendorMicrosoft}
	var h windows.Handle
	r, _, _ := procCreate.Call(uintptr(unsafe.Pointer(&st)), uintptr(unsafe.Pointer(p)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&params)), 0, uintptr(unsafe.Pointer(&h)))
	if err := status(r, "creating virtual disk "+path); err != nil {
		return nil, err
	}
	return attach(h, path, false)
}

func attach(h windows.Handle, path string, readOnly bool) (*Disk, error) {
	d := &Disk{h: h}
	flags := uintptr(attachNoLocalHost | attachNoDriveLetter)
	if readOnly {
		flags |= attachReadOnly
	}
	ap := struct{ Version, Reserved uint32 }{Version: 1}
	r, _, _ := procAttach.Call(uintptr(h), 0, flags, 0, uintptr(unsafe.Pointer(&ap)), 0)
	if err := status(r, "attaching virtual disk "+path); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	// GET_VIRTUAL_DISK_INFO, version 1 (size): the virtual size follows
	// the 8-byte header.
	var info [128]byte
	*(*uint32)(unsafe.Pointer(&info[0])) = 1
	n := uint32(len(info))
	r, _, _ = procInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&info[0])), 0)
	if err := status(r, "reading the size of virtual disk "+path); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	d.size = int64(*(*uint64)(unsafe.Pointer(&info[8])))
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	d.ev = ev
	return d, nil
}

// Size is the size of the disk as a virtual machine sees it.
func (d *Disk) Size() int64 { return d.size }

// Close detaches and closes the disk.
func (d *Disk) Close() error {
	windows.CloseHandle(d.ev)
	return windows.CloseHandle(d.h)
}

// io performs one read or write at an offset. The handle may or may not
// be in overlapped mode, so both ways of completing are handled.
func (d *Disk) io(write bool, p []byte, off int64) (int, error) {
	var done uint32
	o := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32), HEvent: d.ev}
	var err error
	if write {
		err = windows.WriteFile(d.h, p, &done, &o)
	} else {
		err = windows.ReadFile(d.h, p, &done, &o)
	}
	if err == windows.ERROR_IO_PENDING {
		err = windows.GetOverlappedResult(d.h, &o, &done, true)
	}
	return int(done), err
}

// ReadAt reads disk contents. Offsets and lengths must be multiples of
// the sector size. It is not safe for concurrent use.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	n, err := d.io(false, p, off)
	if err == nil && n < len(p) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// WriteAt writes disk contents. Offsets and lengths must be multiples
// of the sector size. It is not safe for concurrent use.
func (d *Disk) WriteAt(p []byte, off int64) (int, error) {
	// Disk devices want sector-aligned memory.
	const align = 4096
	if len(d.buf) < len(p)+align {
		d.buf = make([]byte, len(p)+align)
	}
	skip := (align - int(uintptr(unsafe.Pointer(&d.buf[0]))%align)) % align
	b := d.buf[skip : skip+len(p)]
	copy(b, p)
	return d.io(true, b, off)
}
