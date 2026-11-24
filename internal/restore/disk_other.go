//go:build unix && !linux

package restore

import (
	"errors"

	"github.com/ajthom90/better-backup/internal/disklayout"
)

func openDiskDevice(path string) (*diskTarget, error) {
	return nil, errors.New("restoring a whole machine onto a disk is done from the recovery disc; on this system it can only be restored into a folder")
}

func createVirtualDisk(path string, d *disklayout.Disk) (*diskTarget, error) {
	return nil, errors.New("virtual disks (.vhdx) can only be created on Windows; use a name ending in .img")
}
