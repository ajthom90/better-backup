package restore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	err := r.writeBlocks(e, w, fresh)
	if cerr := done(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w (the destination now holds part of the image and is not usable)", dest, err)
	}
	r.stats.Files++
	return nil
}

// writeBlocks writes an image's blocks to w. fresh says w reads as zeros
// wherever nothing is written, so holes need no writing.
func (r *restorer) writeBlocks(e *tree.Entry, w io.WriterAt, fresh bool) error {
	if e.BlockSize <= 0 {
		return fmt.Errorf("image %s has no block size", e.Name)
	}
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
				r.progress()
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
	return err
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
	children, err := r.children(e)
	if err != nil {
		return err
	}
	if !full {
		return r.pickedImages(children, orig)
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
	target := hypervisor.Target{Place: r.opts.Target, Name: name, OpenFile: openImageTarget, Connection: r.opts.Connection}
	// Over the original: the machine that has the name stays as it is
	// until the disks from the backup are in place. Then it is switched
	// off and renamed, never deleted.
	var replacer hypervisor.Replacer
	var aside string
	if r.opts.VMReplace {
		rp, ok := drv.(hypervisor.Replacer)
		if !ok {
			return fmt.Errorf("virtual machine %s: restoring over the original is not built yet for %s; restore it under a new name instead", e.Name, drv.Label())
		}
		place, found, err := rp.Find(r.ctx, r.opts.Connection, name)
		if err != nil {
			return fmt.Errorf("virtual machine %s: looking for the machine to replace: %w", e.Name, err)
		}
		if found {
			stamp := now().Format("2006-01-02 15.04.05")
			replacer, aside = rp, name+" (replaced "+stamp+")"
			target.Replace, target.Folder = true, name+" restored "+stamp
			if target.Place == "" {
				target.Place = place
			}
			if target.Place == "" {
				return fmt.Errorf("virtual machine %s: the machine to replace has no disk that says where its files are; name a folder to put the restored one in", e.Name)
			}
		}
	}
	rs, err := drv.NewRestore(r.ctx, config, target)
	if err != nil {
		return fmt.Errorf("virtual machine %s: %w", e.Name, err)
	}
	abort := func() {
		if a, ok := rs.(hypervisor.Aborter); ok {
			a.Abort()
		}
	}
	for _, d := range disks {
		spec := hypervisor.DiskSpec{Name: d.Name, Size: d.Size, BlockSize: d.BlockSize}
		err := tree.EachContent(r.get, d, func(id keys.ID) error {
			spec.Data = append(spec.Data, id != keys.ID{})
			return nil
		})
		if err != nil {
			abort()
			return fmt.Errorf("virtual machine %s: its disk %s: %w", e.Name, d.Name, err)
		}
		w, fresh, done, err := rs.Disk(spec)
		if err != nil {
			abort()
			return fmt.Errorf("virtual machine %s: its disk %s: %w", e.Name, d.Name, err)
		}
		if err := r.writeImage(d, d.Name+" of "+name, w, fresh, done); err != nil {
			abort()
			return err
		}
	}
	if replacer != nil {
		if err := replacer.SetAside(r.ctx, r.opts.Connection, name, aside); err != nil {
			abort()
			return fmt.Errorf("virtual machine %s: setting the machine that is there aside failed, so it was left as it is: %w (the disks from the backup were restored to %s)", name, err, rs.Where())
		}
	}
	warnings, err := rs.Finish()
	for _, w := range warnings {
		r.warn("%s: %s", name, w)
	}
	if err != nil {
		err = fmt.Errorf("creating the virtual machine %s: %w (its disks were restored to %s)", name, err, rs.Where())
		if replacer != nil {
			if back := replacer.PutBack(r.ctx, r.opts.Connection, name, aside); back != nil {
				return fmt.Errorf("%w; the machine that was there is now called %q and is switched off, and giving it its name back failed: %v", err, aside, back)
			}
			return fmt.Errorf("%w; the machine that was there has its name back and is switched off: start it again", err)
		}
		return err
	}
	if replacer != nil {
		r.warn("%s: the restored machine's files are in %s. The machine that was there was switched off and is kept as %q. It has the same network addresses as the restored one, so do not run both. Delete it, and its disks, when you are sure of the restored one, and take care to leave that folder alone", name, rs.Where(), aside)
	} else if r.opts.VMReplace {
		r.warn("%s: there was no machine of that name to replace, so it was restored as a new one", name)
	}
	r.stats.Files++
	return nil
}

// now is the clock, which tests replace.
var now = time.Now

// children reads every entry under a virtual machine or whole machine.
func (r *restorer) children(e *tree.Entry) ([]tree.Entry, error) {
	var children []tree.Entry
	for _, p := range e.Subtree {
		page, err := tree.ReadPage(r.get, p)
		if err != nil {
			return nil, err
		}
		children = append(children, page...)
	}
	return children, nil
}

// pickedImages restores the images among children that Include names,
// each as an image on its own.
func (r *restorer) pickedImages(children []tree.Entry, orig string) error {
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
