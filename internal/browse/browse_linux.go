package browse

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/backup"
)

func roots() []api.BrowseEntry { return nil }

func sysText(parts ...string) string {
	b, _ := os.ReadFile(filepath.Join(parts...))
	return strings.TrimSpace(string(b))
}

// skipped are block devices that are never what someone wants to image.
func skipped(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "sr", "fd", "nbd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// volumes lists the block devices with something on them: partitions,
// logical volumes, RAID sets and ZFS volumes.
func volumes() ([]api.BrowseEntry, error) {
	const sys = "/sys/class/block"
	entries, err := os.ReadDir(sys)
	if err != nil {
		return nil, err
	}
	mounts := backup.MountedDevices()
	var out []api.BrowseEntry
	for _, e := range entries {
		name := e.Name()
		// ZFS volumes are listed by their names below.
		if skipped(name) || strings.HasPrefix(name, "zd") {
			continue
		}
		sectors, _ := strconv.ParseInt(sysText(sys, name, "size"), 10, 64)
		if sectors == 0 {
			continue
		}
		dev, kind := "/dev/"+name, "disk"
		if _, err := os.Stat(filepath.Join(sys, name, "partition")); err == nil {
			kind = "partition"
		}
		value := dev
		if dm := sysText(sys, name, "dm", "name"); dm != "" {
			value, kind = "/dev/mapper/"+dm, "logical volume"
		} else if strings.HasPrefix(name, "md") {
			kind = "RAID"
		}
		// A disk that has partitions is offered under "disks", not here.
		if kind == "disk" {
			if parts, _ := filepath.Glob(filepath.Join(sys, name, "*", "partition")); len(parts) > 0 {
				continue
			}
		}
		detail := kind
		if m := mounts[dev]; m != "" {
			detail += ", mounted at " + m
		}
		out = append(out, api.BrowseEntry{Name: value, Value: value, Size: sectors * 512, Detail: detail})
	}
	filepath.WalkDir("/dev/zvol", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(out) >= maxEntries {
			return nil
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil
		}
		// zd16p1 is a partition inside a volume; zd16 the volume.
		detail := "ZFS volume"
		if _, err := os.Stat(filepath.Join(sys, filepath.Base(real), "partition")); err == nil {
			detail = "partition in a ZFS volume"
		}
		sectors, _ := strconv.ParseInt(sysText(sys, filepath.Base(real), "size"), 10, 64)
		out = append(out, api.BrowseEntry{Name: path, Value: path, Size: sectors * 512, Detail: detail})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out, nil
}

// disks lists the whole disks of the machine.
func disks() ([]api.BrowseEntry, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	system := map[string]bool{}
	if sd, err := backup.SystemDisks(); err == nil {
		for _, d := range sd {
			system[d] = true
		}
	}
	var out []api.BrowseEntry
	for _, e := range entries {
		name := e.Name()
		if skipped(name) || strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md") || strings.HasPrefix(name, "zd") {
			continue
		}
		sectors, _ := strconv.ParseInt(sysText("/sys/block", name, "size"), 10, 64)
		if sectors == 0 {
			continue
		}
		dev := "/dev/" + name
		vendor := sysText("/sys/block", name, "device", "vendor")
		if strings.HasPrefix(vendor, "0x") {
			vendor = "" // a virtual disk's number, not a maker's name
		}
		detail := strings.Join(strings.Fields(vendor+" "+sysText("/sys/block", name, "device", "model")), " ")
		if system[dev] {
			if detail != "" {
				detail += ", "
			}
			detail += "the system starts from it"
		}
		out = append(out, api.BrowseEntry{Name: dev, Value: dev, Size: sectors * 512, Detail: detail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out, nil
}
