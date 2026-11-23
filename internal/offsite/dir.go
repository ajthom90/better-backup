package offsite

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type dirTarget struct{ root string }

// NewDir returns a target that keeps the copy in a folder: a second
// disk, a removable drive, or a mounted network share.
func NewDir(path string) (Target, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("the folder for the copy is not set")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &dirTarget{abs}, nil
}

func (d *dirTarget) String() string { return d.root }

func (d *dirTarget) path(name string) (string, error) {
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return "", errors.New("invalid object name " + name)
	}
	return filepath.Join(d.root, filepath.FromSlash(name)), nil
}

func (d *dirTarget) List(ctx context.Context) ([]Object, error) {
	// The folder must already exist: a missing one usually means the
	// disk or share is not mounted, and silently creating it would put
	// the "offsite" copy on the wrong disk.
	if fi, err := os.Stat(d.root); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, errors.New(d.root + " is not a folder")
	}
	var out []Object
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return ctx.Err()
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(d.root, p)
		out = append(out, Object{filepath.ToSlash(rel), fi.Size()})
		return nil
	})
	return out, err
}

func (d *dirTarget) Put(ctx context.Context, name string, r io.ReadSeeker, size int64) error {
	dest, err := d.path(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(d.root); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".tmp-put"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func (d *dirTarget) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	p, err := d.path(name)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (d *dirTarget) Delete(ctx context.Context, name string) error {
	p, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
