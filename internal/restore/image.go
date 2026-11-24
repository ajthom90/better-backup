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

	"github.com/ajthom90/better-backup/internal/hyperv"
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
	err = r.writeImage(e, w, fresh)
	if cerr := done(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w (the destination now holds part of the image and is not usable)", dest, err)
	}
	r.stats.Files++
	return nil
}

// writeImage writes an image's blocks to w. fresh says w reads as zeros
// wherever nothing is written, so holes need no writing.
func (r *restorer) writeImage(e *tree.Entry, w io.WriterAt, fresh bool) error {
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

// vm restores a virtual machine: its disks go into a folder named after
// it under the target, and a new machine is built around them. With
// full false only the disks picked by Include are restored, as images.
func (r *restorer) vm(e *tree.Entry, orig string, full bool) error {
	children, err := r.children(e)
	if err != nil {
		return err
	}
	if !full {
		return r.pickedImages(children, orig)
	}
	if r.opts.Target == "" {
		return fmt.Errorf("restoring the virtual machine %s needs a target folder for its files; it is restored as a new machine beside the original", e.Name)
	}
	name := r.opts.VMName
	if name == "" {
		name = e.Name
	}
	var cfg hyperv.Config
	var disks []*tree.Entry
	for i := range children {
		c := &children[i]
		switch {
		case c.Type == tree.TypeImage && c.Name == filepath.Base(c.Name):
			disks = append(disks, c)
		case c.Type == tree.TypeFile && c.Name == hyperv.ConfigFile:
			var raw []byte
			err := tree.EachContent(r.get, c, func(id keys.ID) error {
				b, err := r.get(id)
				raw = append(raw, b...)
				return err
			})
			if err == nil {
				err = json.Unmarshal(raw, &cfg)
			}
			if err != nil {
				return fmt.Errorf("virtual machine %s: reading its configuration: %w", e.Name, err)
			}
		}
	}
	if cfg.Generation == 0 {
		return fmt.Errorf("virtual machine %s has no configuration in this snapshot", e.Name)
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("the virtual machine %s can only be rebuilt on a Hyper-V host; elsewhere restore its disks one at a time with --path", e.Name)
	}
	safe := strings.Map(func(c rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, c) {
			return '_'
		}
		return c
	}, name)
	dir := filepath.Join(r.opts.Target, safe)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, d := range disks {
		if err := r.imageTo(d, filepath.Join(dir, d.Name)); err != nil {
			return err
		}
	}
	warnings, err := hyperv.Create(cfg, dir, name)
	for _, w := range warnings {
		r.warn("%s: %s", name, w)
	}
	if err != nil {
		return fmt.Errorf("%w (its disks were restored to %s)", err, dir)
	}
	r.stats.Files++
	return nil
}

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
