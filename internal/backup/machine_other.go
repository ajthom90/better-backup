//go:build !linux && !windows

package backup

import "errors"

// PlanMachine is not available here: whole machines are backed up on
// Windows and Linux.
func PlanMachine(disks []string) (*MachinePlan, error) {
	return nil, errors.New("backing up a whole machine for bare-metal recovery is supported on Windows and Linux only")
}

func openSnapshotVolume(name, volume string, mapPath func(string) string) (ImageSource, error) {
	return ImageSource{}, errors.New("volume snapshots are only used on Windows")
}
