package restore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	w, fresh, done, err := openImageTarget(dest, e.Size)
	if err != nil {
		return fmt.Errorf("image %s: %w", e.Name, err)
	}
	off := int64(0)
	var zeros []byte
	err = tree.EachContent(r.get, e, func(id keys.ID) error {
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
