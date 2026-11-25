//go:build !windows && !linux

package browse

import (
	"errors"

	"github.com/ajthom90/better-backup/internal/api"
)

func roots() []api.BrowseEntry { return nil }

func volumes() ([]api.BrowseEntry, error) {
	return nil, errors.New("volumes cannot be listed on this system; type the device, such as /dev/disk2s1")
}

func disks() ([]api.BrowseEntry, error) {
	return nil, errors.New("whole-machine backup is for Windows and Linux machines")
}
