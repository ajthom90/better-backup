package backup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ajthom90/better-backup/internal/disklayout"
)

// PlanMachine looks at the disks this machine starts from. disks names
// them (/dev/sda, or a disk image file); empty means find them from the
// mounted root and boot file systems.
func PlanMachine(disks []string) (*MachinePlan, error) {
	host, _ := os.Hostname()
	p := &MachinePlan{Layout: disklayout.Machine{Hostname: host, OS: "linux", Firmware: disklayout.FirmwareBIOS, Arch: runtime.GOARCH}}
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		p.Layout.Firmware = disklayout.FirmwareUEFI
	}
	if len(disks) == 0 {
		var err error
		if disks, err = systemDisks(); err != nil {
			return nil, err
		}
	}
	mounts := mountedDevices()
	unix.Sync() // so that what is read from the disk is as current as it can be
	for i, path := range disks {
		f, err := os.Open(path)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.closer = append(p.closer, f)
		size, ss, err := DiskGeometry(f)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		d, err := disklayout.Read(f, size, ss)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		d.Name, d.Device = "disk"+strconv.Itoa(i), path
		base := filepath.Base(path)
		if b, err := os.ReadFile("/sys/class/block/" + base + "/device/model"); err == nil {
			d.Model = strings.TrimSpace(string(b))
		}
		p.planDisk(d, f, func(pt *disklayout.Partition) (io.ReaderAt, string) {
			// Read through the partition's own device where there is
			// one: it shows what a mounted file system has written but
			// not yet flushed.
			dev := partitionDevice(base, pt.Number)
			if dev == "" {
				return nil, ""
			}
			pt.Mount = mounts[dev]
			pf, err := os.Open(dev)
			if err != nil {
				return nil, ""
			}
			p.closer = append(p.closer, pf)
			return pf, ""
		})
		for k := range d.Partitions {
			if pt := &d.Partitions[k]; pt.Mount != "" && pt.Image != "" {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s (%s) is mounted and was read while in use; Linux has no snapshot to read it from, so files that changed during the backup may be damaged in it", pt.Mount, pt.Image))
			}
		}
		p.Layout.Disks = append(p.Layout.Disks, *d)
	}
	return p, nil
}

// DiskGeometry returns the size and the logical sector size of a disk.
// For a plain file the sector size is 0: not known.
func DiskGeometry(f *os.File) (size int64, sectorSize int, err error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	if fi.Mode()&os.ModeDevice == 0 {
		return fi.Size(), 0, nil
	}
	var n uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&n))); e != 0 {
		return 0, 0, fmt.Errorf("reading the disk's size: %w", e)
	}
	ss, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKSSZGET)
	if err != nil {
		return 0, 0, fmt.Errorf("reading the disk's sector size: %w", err)
	}
	return int64(n), ss, nil
}

// partitionDevice finds the device of a disk's partition, such as
// /dev/sda2 or /dev/nvme0n1p2.
func partitionDevice(disk string, number int) string {
	entries, _ := os.ReadDir("/sys/class/block/" + disk)
	for _, e := range entries {
		b, err := os.ReadFile("/sys/class/block/" + disk + "/" + e.Name() + "/partition")
		if err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(number) {
			return "/dev/" + e.Name()
		}
	}
	return ""
}

// mountedDevices maps devices to where they are mounted.
func mountedDevices() map[string]string { return MountedDevices() }

// MountedDevices maps devices to where they are mounted.
func MountedDevices() map[string]string {
	out := map[string]string{}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		dev, err := filepath.EvalSymlinks(fields[0])
		if err != nil {
			dev = fields[0]
		}
		if _, ok := out[dev]; !ok {
			out[dev] = fields[1]
		}
	}
	return out
}

// systemDisks finds the disks that hold the root and boot file systems,
// looking through LVM, RAID and encryption layers to the disks beneath.
func systemDisks() ([]string, error) { return SystemDisks() }

// SystemDisks finds the disks this machine starts from.
func SystemDisks() ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	found := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		switch fields[4] {
		case "/", "/boot", "/boot/efi", "/efi":
			wholeDisks("/sys/dev/block/"+fields[2], found, 0)
		}
	}
	if len(found) == 0 {
		return nil, errors.New("could not find the disk this machine starts from; name it in the policy")
	}
	var disks []string
	for d := range found {
		disks = append(disks, d)
	}
	sort.Strings(disks)
	return disks, nil
}

func wholeDisks(sys string, found map[string]bool, depth int) {
	real, err := filepath.EvalSymlinks(sys)
	if err != nil || depth > 8 {
		return
	}
	if _, err := os.Stat(filepath.Join(real, "partition")); err == nil {
		found["/dev/"+filepath.Base(filepath.Dir(real))] = true
		return
	}
	slaves, _ := os.ReadDir(filepath.Join(real, "slaves"))
	if len(slaves) == 0 {
		found["/dev/"+filepath.Base(real)] = true
		return
	}
	for _, s := range slaves {
		wholeDisks(filepath.Join(real, "slaves", s.Name()), found, depth+1)
	}
}

func openSnapshotVolume(name, volume string, mapPath func(string) string) (ImageSource, error) {
	return ImageSource{}, errors.New("volume snapshots are only used on Windows")
}
