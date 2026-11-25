package backup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/disklayout"
)

const (
	ioctlDiskGetGeometryEx = 0x000700A0
	ioctlVolumeDiskExtents = 0x00560000
)

var procGetFirmwareType = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetFirmwareType")

// diskReader reads a physical disk. Disk devices want whole sectors and
// sector-aligned memory, so every read goes through an aligned buffer.
type diskReader struct {
	f   *os.File
	mu  sync.Mutex
	buf []byte
}

func (d *diskReader) ReadAt(p []byte, off int64) (int, error) {
	const align = 4096
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.buf) < len(p)+align {
		d.buf = make([]byte, len(p)+align)
	}
	skip := (align - int(uintptr(unsafe.Pointer(&d.buf[0]))%align)) % align
	b := d.buf[skip : skip+len(p)]
	n, err := d.f.ReadAt(b, off)
	copy(p, b[:n])
	return n, err
}

// volumeInfo is a mounted volume and the one disk extent it occupies.
type volumeInfo struct {
	letter    string // "C:", or empty for a volume without a drive letter
	fs, label string
}

// volumesByExtent finds every volume that lies on a single disk extent,
// keyed by "disk:offset".
func volumesByExtent() map[string]volumeInfo {
	out := map[string]volumeInfo{}
	name := make([]uint16, windows.MAX_PATH+1)
	h, err := windows.FindFirstVolume(&name[0], uint32(len(name)))
	if err != nil {
		return out
	}
	defer windows.FindVolumeClose(h)
	for {
		vol := windows.UTF16ToString(name) // \\?\Volume{...}\
		if disk, off, ok := volumeExtent(strings.TrimRight(vol, `\`)); ok {
			var info volumeInfo
			paths := make([]uint16, 1024)
			var n uint32
			if windows.GetVolumePathNamesForVolumeName(&name[0], &paths[0], uint32(len(paths)), &n) == nil {
				// A list of strings, each ended by a zero.
				for i := 0; i < len(paths) && paths[i] != 0; {
					j := i
					for j < len(paths) && paths[j] != 0 {
						j++
					}
					if p := windows.UTF16ToString(paths[i:j]); len(p) == 3 && p[1] == ':' && info.letter == "" {
						info.letter = strings.ToUpper(p[:2])
					}
					i = j + 1
				}
			}
			label, fs := make([]uint16, windows.MAX_PATH+1), make([]uint16, windows.MAX_PATH+1)
			if windows.GetVolumeInformation(&name[0], &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs))) == nil {
				info.label, info.fs = windows.UTF16ToString(label), windows.UTF16ToString(fs)
			}
			out[fmt.Sprintf("%d:%d", disk, off)] = info
		}
		if windows.FindNextVolume(h, &name[0], uint32(len(name))) != nil {
			return out
		}
	}
}

// volumeExtent returns the disk and offset of a volume that lies on
// exactly one disk extent.
func volumeExtent(dev string) (disk uint32, off int64, ok bool) {
	p, err := windows.UTF16PtrFromString(dev)
	if err != nil {
		return 0, 0, false
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, 0, false
	}
	defer windows.CloseHandle(h)
	var ext struct {
		Count  uint32
		_      uint32
		Disk   uint32
		_      uint32
		Start  int64
		Length int64
	}
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlVolumeDiskExtents, nil, 0, (*byte)(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), &n, nil); err != nil || ext.Count != 1 {
		return 0, 0, false
	}
	return ext.Disk, ext.Start, true
}

// DiskOfVolume says which disk a drive such as "C:" lies on, if it lies
// on one.
func DiskOfVolume(letter string) (uint32, bool) {
	n, _, ok := volumeExtent(`\\.\` + letter)
	return n, ok
}

// SystemDisk is the number of the disk that holds Windows.
func SystemDisk() (uint32, bool) {
	sys := os.Getenv("SystemDrive")
	if len(sys) != 2 {
		sys = "C:"
	}
	return DiskOfVolume(sys)
}

// DiskSize returns the size of the disk with this number, or false if
// there is none.
func DiskSize(number uint32) (int64, bool) {
	dp, _ := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	h, err := windows.CreateFile(dp, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	// DISK_GEOMETRY_EX: the disk size is at offset 24.
	var geo [256]byte
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlDiskGetGeometryEx, nil, 0, &geo[0], uint32(len(geo)), &n, nil); err != nil {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(geo[24:])), true
}

// PlanMachine looks at the disks this machine starts from. disks names
// them by number ("0") or device (\\.\PhysicalDrive0); empty means the
// disk that holds Windows.
func PlanMachine(disks []string) (*MachinePlan, error) {
	host, _ := os.Hostname()
	p := &MachinePlan{Layout: disklayout.Machine{Hostname: host, OS: "windows", Arch: runtime.GOARCH}}
	var fw uint32
	if procGetFirmwareType.Find() == nil {
		if r, _, _ := procGetFirmwareType.Call(uintptr(unsafe.Pointer(&fw))); r != 0 {
			switch fw {
			case 1:
				p.Layout.Firmware = disklayout.FirmwareBIOS
			case 2:
				p.Layout.Firmware = disklayout.FirmwareUEFI
			}
		}
	}
	var numbers []uint32
	if len(disks) == 0 {
		sys := os.Getenv("SystemDrive")
		if len(sys) != 2 {
			sys = "C:"
		}
		n, _, ok := volumeExtent(`\\.\` + sys)
		if !ok {
			return nil, fmt.Errorf("could not find the disk that holds %s; it may span several disks. Name the disks in the policy", sys)
		}
		numbers = []uint32{n}
	}
	for _, d := range disks {
		s := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), `\\.\physicaldrive`)
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not a disk: give its number, such as 0", d)
		}
		numbers = append(numbers, uint32(n))
	}
	vols := volumesByExtent()
	for i, num := range numbers {
		dev := fmt.Sprintf(`\\.\PhysicalDrive%d`, num)
		dp, _ := windows.UTF16PtrFromString(dev)
		h, err := windows.CreateFile(dp, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("opening %s: %w", dev, err)
		}
		f := os.NewFile(uintptr(h), dev)
		p.closer = append(p.closer, f)
		// DISK_GEOMETRY_EX: bytes per sector at offset 20, disk size at 24.
		var geo [256]byte
		var n uint32
		if err := windows.DeviceIoControl(h, ioctlDiskGetGeometryEx, nil, 0, &geo[0], uint32(len(geo)), &n, nil); err != nil {
			p.Close()
			return nil, fmt.Errorf("reading the geometry of %s: %w", dev, err)
		}
		ss, size := int(*(*uint32)(unsafe.Pointer(&geo[20]))), *(*int64)(unsafe.Pointer(&geo[24]))
		r := &diskReader{f: f}
		d, err := disklayout.Read(r, size, ss)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("%s: %w", dev, err)
		}
		d.Name, d.Device = "disk"+strconv.Itoa(i), dev
		p.planDisk(d, r, func(pt *disklayout.Partition) (io.ReaderAt, string) {
			switch pt.Type {
			case "42", "5808C8AA-7E8F-42E0-85D2-E1E90434CFB3", "AF9B60A0-1431-4F62-BC68-3311714A69AD":
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s is a dynamic disk; a restore puts its contents back, but dynamic disks have not been tested", dev))
			}
			v, ok := vols[fmt.Sprintf("%d:%d", num, pt.Offset)]
			if !ok {
				return nil, ""
			}
			pt.FS, pt.Label, pt.Mount = v.fs, v.label, v.letter
			// A volume in use is only usable as an image when it is read
			// from a snapshot. Volumes without a drive letter (the EFI
			// system partition, the recovery partition) are quiet, are
			// often too small or of the wrong kind for a snapshot, and
			// are read as they are.
			if v.letter != "" && (strings.EqualFold(v.fs, "NTFS") || strings.EqualFold(v.fs, "ReFS")) {
				return nil, v.letter + `\`
			}
			return nil, ""
		})
		p.Layout.Disks = append(p.Layout.Disks, *d)
	}
	for _, part := range p.parts {
		if part.volume != "" {
			p.SnapPaths = append(p.SnapPaths, part.volume)
		}
	}
	return p, nil
}

func openSnapshotVolume(name, volume string, mapPath func(string) string) (ImageSource, error) {
	dev := strings.TrimRight(mapPath(volume), `\`)
	if strings.EqualFold(dev, strings.TrimRight(volume, `\`)) {
		return ImageSource{}, errors.New("volume " + strings.TrimRight(volume, `\`) + " has no VSS snapshot, and an image of a volume in use is only good when it is read from one")
	}
	return OpenImage(name, dev)
}
