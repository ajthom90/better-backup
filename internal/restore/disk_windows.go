package restore

import (
	"errors"
	"fmt"

	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/vhd"
)

func openDiskDevice(path string) (*diskTarget, error) {
	return nil, errors.New("restoring a whole machine onto a physical disk is done from the recovery disc; on a running Windows system it can only be restored into a folder, as virtual disks")
}

// createVirtualDisk makes a new VHDX the size of the original disk.
func createVirtualDisk(path string, d *disklayout.Disk) (*diskTarget, error) {
	if d.SectorSize != 512 {
		return nil, fmt.Errorf("the disk had %d-byte sectors; virtual disks are made with 512-byte sectors, which this layout cannot be put on", d.SectorSize)
	}
	v, err := vhd.Create(path, (d.Size+511)/512*512)
	if err != nil {
		return nil, err
	}
	return &diskTarget{w: v, size: v.Size(), sectorSize: 512, fresh: true, done: v.Close}, nil
}
