package server

import "golang.org/x/sys/windows"

// diskSpace reports the size of the volume holding path and how much of
// it is free.
func diskSpace(path string) (total, free int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, size, all uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &size, &all); err != nil {
		return 0, 0, err
	}
	return int64(size), int64(avail), nil
}
