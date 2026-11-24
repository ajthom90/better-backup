package restore

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ajthom90/better-backup/internal/disklayout"
)

// openDiskDevice opens a whole disk for writing. Opening it exclusively
// fails while any of its partitions is mounted or otherwise in use.
func openDiskDevice(path string) (*diskTarget, error) {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_EXCL, 0)
	if errors.Is(err, unix.EBUSY) {
		return nil, fmt.Errorf("%s is in use: one of its partitions is mounted or part of a volume group. A whole machine can only be restored onto a disk nothing is using", path)
	}
	if err != nil {
		return nil, err
	}
	var size uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("reading the size of %s: %w", path, e)
	}
	ss, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKSSZGET)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("reading the sector size of %s: %w", path, err)
	}
	return &diskTarget{w: f, size: int64(size), sectorSize: ss, done: func() error {
		err := f.Sync()
		// Have the kernel read the new partition table. It does not
		// matter if it cannot: the machine is restarted anyway.
		unix.IoctlSetInt(int(f.Fd()), unix.BLKRRPART, 0)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}}, nil
}

func createVirtualDisk(path string, d *disklayout.Disk) (*diskTarget, error) {
	return nil, errors.New("virtual disks (.vhdx) can only be created on Windows; use a name ending in .img")
}
