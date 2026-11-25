package browse

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/backup"
)

// drives lists the drive letters of the given kinds.
func drives(kinds ...uint32) []api.BrowseEntry {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return []api.BrowseEntry{}
	}
	names := map[uint32]string{windows.DRIVE_REMOVABLE: "removable", windows.DRIVE_REMOTE: "network drive", windows.DRIVE_CDROM: "disc"}
	out := []api.BrowseEntry{}
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + ":"
		root, _ := windows.UTF16PtrFromString(letter + `\`)
		kind := windows.GetDriveType(root)
		wanted := false
		for _, k := range kinds {
			wanted = wanted || k == kind
		}
		if !wanted {
			continue
		}
		e := api.BrowseEntry{Name: letter, Value: letter, Dir: true}
		var parts []string
		label, fs := make([]uint16, windows.MAX_PATH+1), make([]uint16, windows.MAX_PATH+1)
		if windows.GetVolumeInformation(root, &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs))) == nil {
			if l := windows.UTF16ToString(label); l != "" {
				parts = append(parts, l)
			}
			if f := windows.UTF16ToString(fs); f != "" {
				parts = append(parts, f)
			}
		}
		if n := names[kind]; n != "" {
			parts = append(parts, n)
		}
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(root, &free, &total, &totalFree) == nil {
			e.Size = int64(total)
		}
		e.Detail = strings.Join(parts, ", ")
		out = append(out, e)
	}
	return out
}

// roots are where browsing for folders starts: the drives.
func roots() []api.BrowseEntry {
	out := drives(windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_REMOTE)
	for i := range out {
		out[i].Value += `\`
	}
	return out
}

// volumes are the drives that can be imaged.
func volumes() ([]api.BrowseEntry, error) {
	out := drives(windows.DRIVE_FIXED)
	for i := range out {
		out[i].Dir = false
	}
	return out, nil
}

// disks lists the physical disks by number, with the drives on each.
func disks() ([]api.BrowseEntry, error) {
	letters := map[uint32][]string{}
	for _, d := range drives(windows.DRIVE_FIXED) {
		if n, ok := backup.DiskOfVolume(d.Value); ok {
			letters[n] = append(letters[n], d.Value)
		}
	}
	system, hasSystem := backup.SystemDisk()
	out := []api.BrowseEntry{}
	// Disk numbers can have gaps after a disk was removed.
	for n := uint32(0); n < 64; n++ {
		size, ok := backup.DiskSize(n)
		if !ok {
			continue
		}
		var parts []string
		if l := letters[n]; len(l) > 0 {
			sort.Strings(l)
			parts = append(parts, "holds "+strings.Join(l, ", "))
		}
		if hasSystem && system == n {
			parts = append(parts, "Windows starts from it")
		}
		out = append(out, api.BrowseEntry{Name: fmt.Sprintf("Disk %d", n), Value: fmt.Sprint(n), Size: size, Detail: strings.Join(parts, ", ")})
	}
	return out, nil
}
