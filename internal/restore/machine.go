package restore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/hyperv"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// diskTarget is a disk, or a file standing in for one, that receives a
// whole disk of a machine.
type diskTarget struct {
	w    io.WriterAt
	size int64
	// sectorSize is 0 when the target has no sectors of its own (a file).
	sectorSize int
	// fresh says the target reads as zeros wherever nothing is written.
	fresh bool
	done  func() error
}

// offsetWriter writes into a partition of a disk.
type offsetWriter struct {
	w    io.WriterAt
	base int64
}

func (o offsetWriter) WriteAt(p []byte, off int64) (int, error) { return o.w.WriteAt(p, o.base+off) }

func safeName(s string) string {
	return strings.Map(func(c rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, c) {
			return '_'
		}
		return c
	}, s)
}

// ReadMachine returns the disk layout kept in a whole-machine entry and
// the images of its partitions by name.
func ReadMachine(get tree.Getter, e *tree.Entry) (*disklayout.Machine, map[string]*tree.Entry, error) {
	var m *disklayout.Machine
	images := map[string]*tree.Entry{}
	for _, p := range e.Subtree {
		page, err := tree.ReadPage(get, p)
		if err != nil {
			return nil, nil, err
		}
		for i := range page {
			c := &page[i]
			switch {
			case c.Type == tree.TypeImage:
				images[c.Name] = c
			case c.Type == tree.TypeFile && c.Name == disklayout.FileName:
				var raw []byte
				err := tree.EachContent(get, c, func(id keys.ID) error {
					b, err := get(id)
					raw = append(raw, b...)
					return err
				})
				if err == nil {
					m = new(disklayout.Machine)
					err = json.Unmarshal(raw, m)
				}
				if err != nil {
					return nil, nil, fmt.Errorf("reading the machine's disk layout: %w", err)
				}
			}
		}
	}
	if m == nil || len(m.Disks) == 0 {
		return nil, nil, errors.New("this backup holds no disk layout for the machine")
	}
	return m, images, nil
}

// machine restores a whole machine: every disk gets its partitions'
// contents and then its partition table, so it starts like the original.
// With full false only the partitions picked by Include are restored,
// as images.
func (r *restorer) machine(e *tree.Entry, orig string, full bool) error {
	if !full {
		children, err := r.children(e)
		if err != nil {
			return err
		}
		return r.pickedImages(children, orig)
	}
	m, images, err := ReadMachine(r.get, e)
	if err != nil {
		return err
	}
	if len(r.opts.Disks) == 0 && r.opts.Target == "" {
		return errors.New("a whole machine is restored onto blank disks from the recovery disc, or into a folder as virtual disks; it cannot be restored over the running system")
	}
	dir, ext := r.opts.Target, ".img"
	if runtime.GOOS == "windows" {
		ext = ".vhdx"
	}
	makeVM := len(r.opts.Disks) == 0 && r.opts.VMName != ""
	if makeVM {
		if runtime.GOOS != "windows" {
			return errors.New("a virtual machine can only be built on a Hyper-V host; leave the machine name empty to get the disks alone")
		}
		dir = filepath.Join(dir, safeName(r.opts.VMName))
	}
	if len(r.opts.Disks) == 0 {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	used := map[string]string{}
	var files []string
	for i := range m.Disks {
		d := &m.Disks[i]
		dest := r.opts.Disks[d.Name]
		switch {
		case len(r.opts.Disks) == 0 && makeVM:
			dest = filepath.Join(dir, d.Name+ext)
		case len(r.opts.Disks) == 0:
			dest = filepath.Join(dir, safeName(m.Hostname)+"-"+d.Name+ext)
		case dest == "":
			r.warn("%s of the backup was left out: no disk was chosen for it", d.Name)
			continue
		}
		if other, dup := used[dest]; dup {
			return fmt.Errorf("%s and %s cannot both go to %s", other, d.Name, dest)
		}
		used[dest] = d.Name
		if err := r.disk(d, images, dest); err != nil {
			return err
		}
		files = append(files, filepath.Base(dest))
	}
	if len(files) == 0 {
		return errors.New("no disk of the backup was given a disk to go to")
	}
	if makeVM {
		cfg := vmFor(m, files)
		warnings, err := hyperv.Create(cfg, dir, r.opts.VMName)
		for _, w := range warnings {
			r.warn("%s: %s", r.opts.VMName, w)
		}
		if err != nil {
			return fmt.Errorf("%w (the machine's disks were restored to %s)", err, dir)
		}
		r.warn("%s was created switched off and without a network connection, so that it cannot clash with the original machine. Connect its network adapter when you are ready", r.opts.VMName)
	}
	return nil
}

// vmFor describes a Hyper-V machine that starts from restored disks.
func vmFor(m *disklayout.Machine, files []string) hyperv.Config {
	cfg := hyperv.Config{Name: m.Hostname, Generation: 1, Processors: 2, MemoryStartup: 4 << 30,
		NICs:  []hyperv.NIC{{Name: "Network Adapter", DynamicMAC: true}},
		Notes: "Restored by better-backup from a whole-machine backup of " + m.Hostname + "."}
	// A machine that started through UEFI needs a generation 2 machine;
	// one with an MBR boot disk needs generation 1 and its IDE controller.
	if m.Firmware == disklayout.FirmwareUEFI || (m.Firmware == "" && m.Disks[0].Style == disklayout.StyleGPT) {
		cfg.Generation = 2
		if m.OS == "windows" {
			cfg.SecureBoot, cfg.SecureTemplate = true, "MicrosoftWindows"
		}
	}
	for i, f := range files {
		d := hyperv.Disk{ControllerType: "SCSI", ControllerLocation: i, File: f}
		if cfg.Generation == 1 {
			// Two IDE controllers with two places each; the rest on SCSI.
			if i < 4 {
				d.ControllerType, d.ControllerNumber, d.ControllerLocation = "IDE", i/2, i%2
			} else {
				d.ControllerLocation = i - 4
			}
		}
		cfg.Disks = append(cfg.Disks, d)
	}
	return cfg
}

// disk writes one disk of a machine to dest.
func (r *restorer) disk(d *disklayout.Disk, images map[string]*tree.Entry, dest string) (err error) {
	for i := range d.Partitions {
		p := &d.Partitions[i]
		if p.Image == "" {
			continue
		}
		img := images[p.Image]
		if img == nil {
			return fmt.Errorf("%s: the backup does not hold the image %s of partition %d", d.Name, p.Image, p.Number)
		}
		if img.Size > p.Size {
			return fmt.Errorf("%s: the image %s is larger than its partition", d.Name, p.Image)
		}
	}
	t, err := openDiskTarget(dest, d)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Name, err)
	}
	wrote := false
	defer func() {
		if cerr := t.done(); err == nil {
			err = cerr
		}
		if err != nil && wrote {
			err = fmt.Errorf("writing %s to %s: %w (the disk now holds part of a restore and is not usable)", d.Name, dest, err)
		} else if err != nil {
			err = fmt.Errorf("%s: %w", d.Name, err)
		}
	}()
	if err := d.Check(t.size, t.sectorSize); err != nil {
		return fmt.Errorf("%s: %w", dest, err)
	}
	const mib = 1 << 20
	zeros := make([]byte, mib)
	wipe := func(off, n int64) error {
		_, err := t.w.WriteAt(zeros[:n], off)
		return err
	}
	wrote = true
	if !t.fresh {
		// Remove the old partition table, and its spare copy at the end,
		// before anything else: a disk that is only partly restored must
		// not look like a usable one.
		if err := wipe(0, mib); err != nil {
			return err
		}
		if err := wipe((t.size-mib)/mib*mib, mib); err != nil {
			return err
		}
	}
	for i := range d.Partitions {
		p := &d.Partitions[i]
		if p.Kind == disklayout.KindExtended {
			continue
		}
		if p.Image == "" {
			// Nothing was kept of it. Clear its start so that whatever
			// the disk held there before is not taken for a file system.
			if !t.fresh {
				if err := wipe(p.Offset, min(p.Size, mib)); err != nil {
					return err
				}
			}
			continue
		}
		if err := r.writeImage(images[p.Image], offsetWriter{t.w, p.Offset}, t.fresh); err != nil {
			return err
		}
	}
	if err := d.Write(t.w, t.size); err != nil {
		return err
	}
	r.stats.Files++
	return nil
}

// openDiskTarget opens the device, or creates the file, a disk goes to.
func openDiskTarget(dest string, d *disklayout.Disk) (*diskTarget, error) {
	if isDevice(dest) {
		return openDiskDevice(dest)
	}
	if _, err := os.Lstat(dest); err == nil {
		return nil, fmt.Errorf("%s already exists; choose another folder or name", dest)
	}
	if strings.EqualFold(filepath.Ext(dest), ".vhdx") {
		return createVirtualDisk(dest, d)
	}
	w, _, done, err := createImageFile(dest, d.Size)
	if err != nil {
		return nil, err
	}
	return &diskTarget{w: w, size: d.Size, fresh: true, done: done}, nil
}
