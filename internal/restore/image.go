package restore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	_ "github.com/ajthom90/better-backup/internal/hypervisor/all"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// image restores a volume or disk image. In place it writes to the
// original device. With a target it writes to that device, or to that
// file, or to a file named after the image inside that directory.
func (r *restorer) image(e *tree.Entry) error {
	if e.BlockSize <= 0 {
		return fmt.Errorf("image %s has no block size", e.Name)
	}
	dest := e.Name
	if t := r.opts.Target; t != "" {
		dest = t
		if fi, err := os.Stat(t); err == nil && fi.IsDir() && !isDevice(t) {
			name := strings.Map(func(c rune) rune {
				if strings.ContainsRune(`/\:*?"<>|`, c) {
					return '_'
				}
				return c
			}, strings.Trim(e.Name, `/\:`))
			dest = filepath.Join(t, name+".img")
		} else if r.imageDest {
			return errors.New("the snapshot holds several images; restore one at a time with --path, or choose a directory as the target")
		}
		r.imageDest = true
	}
	return r.imageTo(e, dest)
}

// imageTo writes an image to dest: a device, or a file to create.
func (r *restorer) imageTo(e *tree.Entry, dest string) error {
	w, fresh, done, err := openImageTarget(dest, e.Size)
	if err != nil {
		return fmt.Errorf("image %s: %w", e.Name, err)
	}
	return r.writeImage(e, dest, w, fresh, done)
}

// writeImage writes an image's blocks to w. fresh says w already reads
// as zeros; done is called after the last write.
func (r *restorer) writeImage(e *tree.Entry, dest string, w io.WriterAt, fresh bool, done func() error) error {
	off := int64(0)
	var zeros []byte
	err := tree.EachContent(r.get, e, func(id keys.ID) error {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n := min(e.BlockSize, e.Size-off)
		if n <= 0 {
			return fmt.Errorf("image %s lists more blocks than its size", e.Name)
		}
		if id == (keys.ID{}) {
			// A hole. A new file or virtual disk already reads as zeros
			// there; an existing device must be cleared if the zeros
			// are part of the image.
			if e.HolesZero && !fresh {
				if zeros == nil {
					zeros = make([]byte, e.BlockSize)
				}
				if _, err := w.WriteAt(zeros[:n], off); err != nil {
					return err
				}
			}
		} else {
			data, err := r.get(id)
			if err != nil {
				return err
			}
			if int64(len(data)) != n {
				return fmt.Errorf("image %s: block at offset %d has %d bytes, want %d", e.Name, off, len(data), n)
			}
			if _, err := w.WriteAt(data, off); err != nil {
				return err
			}
			r.stats.Bytes += n
			r.progress()
		}
		off += n
		return nil
	})
	if err == nil && off != e.Size {
		err = fmt.Errorf("image %s: restored %d bytes but the backup recorded %d", e.Name, off, e.Size)
	}
	if cerr := done(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w (the destination now holds part of the image and is not usable)", dest, err)
	}
	r.stats.Files++
	return nil
}

// createImageFile makes a sparse file of the image's size.
func createImageFile(path string, size int64) (io.WriterAt, bool, func() error, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, false, nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, false, nil, err
	}
	return f, true, f.Close, nil
}

// vm restores a virtual machine: the driver of the hypervisor that was
// backed up says where its disks go and builds a new machine around them. With
// full false only the disks picked by Include are restored, as images.
func (r *restorer) vm(e *tree.Entry, orig string, full bool) error {
	var children []tree.Entry
	for _, p := range e.Subtree {
		page, err := tree.ReadPage(r.get, p)
		if err != nil {
			return err
		}
		children = append(children, page...)
	}
	if !full {
		for i := range children {
			c := &children[i]
			if c.Type == tree.TypeImage && r.match(r.join(orig, c.Name)) == matchFull {
				if err := r.image(c); err != nil {
					return err
				}
			}
		}
		return nil
	}
	name := r.opts.VMName
	if name == "" {
		name = e.Name
	}
	var drv hypervisor.Driver
	var config []byte
	var disks []*tree.Entry
	for i := range children {
		c := &children[i]
		switch {
		case c.Type == tree.TypeImage && c.Name == filepath.Base(c.Name):
			disks = append(disks, c)
		case c.Type == tree.TypeFile && drv == nil && hypervisor.ByConfigFile(c.Name) != nil:
			drv = hypervisor.ByConfigFile(c.Name)
			err := tree.EachContent(r.get, c, func(id keys.ID) error {
				b, err := r.get(id)
				config = append(config, b...)
				return err
			})
			if err != nil {
				return fmt.Errorf("virtual machine %s: reading its configuration: %w", e.Name, err)
			}
		}
	}
	if drv == nil {
		return fmt.Errorf("virtual machine %s has no configuration in this snapshot that this agent version understands; restore its disks one at a time with --path", e.Name)
	}
	if err := drv.Available(); err != nil {
		return fmt.Errorf("the virtual machine %s can only be rebuilt on a %s host; elsewhere restore its disks one at a time with --path", e.Name, drv.Label())
	}
	rs, err := drv.NewRestore(r.ctx, config, hypervisor.Target{Place: r.opts.Target, Name: name, OpenFile: openImageTarget})
	if err != nil {
		return fmt.Errorf("virtual machine %s: %w", e.Name, err)
	}
	for _, d := range disks {
		w, fresh, done, err := rs.Disk(d.Name, d.Size)
		if err != nil {
			return fmt.Errorf("virtual machine %s: its disk %s: %w", e.Name, d.Name, err)
		}
		if err := r.writeImage(d, d.Name+" of "+name, w, fresh, done); err != nil {
			return err
		}
	}
	warnings, err := rs.Finish()
	for _, w := range warnings {
		r.warn("%s: %s", name, w)
	}
	if err != nil {
		return fmt.Errorf("creating the virtual machine %s: %w (its disks were restored to %s)", name, err, rs.Where())
	}
	r.stats.Files++
	return nil
}
