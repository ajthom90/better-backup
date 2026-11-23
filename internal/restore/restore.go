// Package restore writes a snapshot, or part of one, back to disk: either
// over the original paths (restore in place) or under a target directory.
package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// Overwrite policies for files that already exist at the destination.
const (
	OverwriteIfChanged = "if-changed" // replace unless size and mtime match the backup
	OverwriteAlways    = "always"
	OverwriteNever     = "never"
)

// Options configures a restore.
type Options struct {
	Sources  []tree.Entry
	SourceOS string
	// Include limits the restore to these original paths and everything
	// under them. Empty restores everything.
	Include []string
	// Target is the directory to restore under. Empty restores in place.
	Target    string
	Overwrite string
	Progress  func(Stats)
	Warn      func(string)
}

// Stats summarizes a restore.
type Stats struct {
	Files    int64 `json:"files"`
	Dirs     int64 `json:"dirs"`
	Bytes    int64 `json:"bytes"`
	Skipped  int64 `json:"skipped"`
	Warnings int64 `json:"warnings"`
}

type restorer struct {
	ctx   context.Context
	get   tree.Getter
	opts  Options
	sep   string
	fold  bool
	stats Stats
	last  time.Time
	// imageDest is set once an image was written to the target itself.
	imageDest bool
}

// Run performs the restore.
func Run(ctx context.Context, get tree.Getter, opts Options) (Stats, error) {
	r := &restorer{ctx: ctx, get: get, opts: opts, sep: "/"}
	if opts.SourceOS == "windows" {
		r.sep, r.fold = `\`, true
	}
	switch opts.Overwrite {
	case "":
		r.opts.Overwrite = OverwriteIfChanged
	case OverwriteIfChanged, OverwriteAlways, OverwriteNever:
	default:
		return r.stats, fmt.Errorf("unknown overwrite policy %q", opts.Overwrite)
	}
	if opts.Target == "" && opts.SourceOS != runtime.GOOS {
		return r.stats, fmt.Errorf("cannot restore a %s backup in place on %s; choose a target directory", opts.SourceOS, runtime.GOOS)
	}
	for i, inc := range r.opts.Include {
		if len(inc) > 1 && !strings.HasSuffix(inc, ":"+r.sep) {
			r.opts.Include[i] = strings.TrimRight(inc, r.sep)
		}
	}
	matched := false
	for i := range opts.Sources {
		e := &opts.Sources[i]
		m := r.match(e.Name)
		if m == matchNone {
			continue
		}
		matched = true
		if e.Name == tree.SystemStateName && e.Type == tree.TypeDir && opts.Target == "" {
			r.warn("the system state was not restored: it cannot be restored over a running system yet. Restore it to a folder instead")
			continue
		}
		if err := r.entry(e, e.Name, r.dest(e.Name), m == matchFull); err != nil {
			return r.stats, err
		}
	}
	if !matched {
		return r.stats, errors.New("nothing in the snapshot matches the requested paths")
	}
	return r.stats, nil
}

const (
	matchNone = iota
	matchAncestor
	matchFull
)

func (r *restorer) under(path, dir string) bool {
	if r.fold {
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	if path == dir {
		return true
	}
	if !strings.HasSuffix(dir, r.sep) {
		dir += r.sep
	}
	return strings.HasPrefix(path, dir)
}

func (r *restorer) match(orig string) int {
	if len(r.opts.Include) == 0 {
		return matchFull
	}
	res := matchNone
	for _, inc := range r.opts.Include {
		if r.under(orig, inc) {
			return matchFull
		}
		if r.under(inc, orig) {
			res = matchAncestor
		}
	}
	return res
}

func (r *restorer) join(dir, name string) string {
	if strings.HasSuffix(dir, r.sep) {
		return dir + name
	}
	return dir + r.sep + name
}

// dest maps a source root's original path to where it is restored.
func (r *restorer) dest(orig string) string {
	if r.opts.Target == "" {
		return orig
	}
	rel := orig
	if r.sep == `\` {
		rel = strings.ReplaceAll(strings.ReplaceAll(rel, ":", ""), `\`, "/")
	}
	return filepath.Join(r.opts.Target, filepath.FromSlash(rel))
}

func (r *restorer) warn(format string, args ...any) {
	r.stats.Warnings++
	if r.opts.Warn != nil {
		r.opts.Warn(fmt.Sprintf(format, args...))
	}
}

func (r *restorer) progress() {
	if r.opts.Progress != nil && time.Since(r.last) > 2*time.Second {
		r.last = time.Now()
		r.opts.Progress(r.stats)
	}
}

// entry restores e to dest. full is false for directories that are only
// traversed to reach an included path; their metadata is left alone.
func (r *restorer) entry(e *tree.Entry, orig, dest string, full bool) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	switch e.Type {
	case tree.TypeDir:
		created := false
		if fi, err := os.Lstat(dest); err != nil {
			if err := os.MkdirAll(dest, 0o700); err != nil {
				r.warn("%v", err)
				return nil
			}
			created = true
		} else if !fi.IsDir() {
			r.warn("%s exists and is not a directory; skipped", dest)
			return nil
		}
		for _, p := range e.Subtree {
			children, err := tree.ReadPage(r.get, p)
			if err != nil {
				return err
			}
			for i := range children {
				c := &children[i]
				if c.Name == "" || c.Name == "." || c.Name == ".." || strings.ContainsAny(c.Name, `/\`) {
					r.warn("skipped entry with unsafe name %q in %s", c.Name, orig)
					continue
				}
				co := r.join(orig, c.Name)
				m := matchFull
				if !full {
					if m = r.match(co); m == matchNone {
						continue
					}
				}
				if err := r.entry(c, co, filepath.Join(dest, c.Name), m == matchFull); err != nil {
					return err
				}
			}
		}
		if full || created {
			if err := r.streams(dest, e); err != nil {
				if cerr := r.ctx.Err(); cerr != nil {
					return cerr
				}
				r.warn("%s: restoring alternate data streams: %v", dest, err)
			}
			r.applyMeta(dest, e)
		}
		if full {
			r.stats.Dirs++
		}
	case tree.TypeFile:
		return r.file(e, dest)
	case tree.TypeImage:
		return r.image(e)
	case tree.TypeSymlink:
		if _, err := os.Lstat(dest); err == nil {
			if r.opts.Overwrite == OverwriteNever {
				r.stats.Skipped++
				return nil
			}
			if cur, err := os.Readlink(dest); err == nil && cur == e.LinkTarget {
				r.stats.Skipped++
				return nil
			}
			if err := os.Remove(dest); err != nil {
				r.warn("%v", err)
				return nil
			}
		}
		if err := os.Symlink(e.LinkTarget, dest); err != nil {
			r.warn("%v", err)
			return nil
		}
		r.chown(dest, e)
		r.stats.Files++
	}
	return nil
}

func (r *restorer) file(e *tree.Entry, dest string) error {
	if fi, err := os.Lstat(dest); err == nil {
		switch {
		case fi.IsDir():
			r.warn("%s exists and is a directory; skipped", dest)
			return nil
		case r.opts.Overwrite == OverwriteNever,
			r.opts.Overwrite == OverwriteIfChanged && fi.Mode().IsRegular() && fi.Size() == e.Size && fi.ModTime().UnixNano() == e.ModTime:
			r.stats.Skipped++
			return nil
		}
	}
	// Write beside the destination and rename, so an interrupted restore
	// never leaves a half-written file under the real name.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".bb-restore-*")
	if err != nil {
		r.warn("%v", err)
		return nil
	}
	var n int64
	err = tree.EachContent(r.get, e, func(id keys.ID) error {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		data, err := r.get(id)
		if err != nil {
			return err
		}
		n += int64(len(data))
		r.stats.Bytes += int64(len(data))
		r.progress()
		_, err = tmp.Write(data)
		return err
	})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != e.Size {
		err = fmt.Errorf("restored %d bytes but the backup recorded %d", n, e.Size)
	}
	if err == nil {
		// Streams go on the temp file: they travel with it on rename.
		if err = r.streams(tmp.Name(), e); err != nil && r.ctx.Err() == nil {
			r.warn("%s: restoring alternate data streams: %v", dest, err)
			err = nil
		}
	}
	if err == nil {
		r.applyMeta(tmp.Name(), e)
		if err = os.Rename(tmp.Name(), dest); err != nil {
			// Windows refuses to replace a read-only file.
			if os.Chmod(dest, 0o600) == nil {
				err = os.Rename(tmp.Name(), dest)
			}
		}
	}
	if err != nil {
		os.Remove(tmp.Name())
		if cerr := r.ctx.Err(); cerr != nil {
			return cerr
		}
		r.warn("%s: %v", dest, err)
		return nil
	}
	r.stats.Files++
	return nil
}

func (r *restorer) applyMeta(path string, e *tree.Entry) {
	r.chown(path, e)
	if err := os.Chmod(path, tree.FileMode(e.Mode)); err != nil {
		r.warn("%v", err)
	}
	mt := time.Unix(0, e.ModTime)
	if err := os.Chtimes(path, mt, mt); err != nil {
		r.warn("%v", err)
	}
	// Last: restored permissions may be tighter than what the steps
	// above need.
	r.platformMeta(path, e)
}

// streams writes an entry's NTFS alternate data streams onto path. They
// only exist on Windows; elsewhere they are left out of the restore.
func (r *restorer) streams(path string, e *tree.Entry) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	for _, st := range e.Streams {
		if st.Name == "" || strings.ContainsAny(st.Name, `/\:`) {
			r.warn("%s: skipped a stream with an unsafe name %q", path, st.Name)
			continue
		}
		f, err := os.OpenFile(path+":"+st.Name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		err = tree.EachID(r.get, st.Content, st.ContentIndex, func(id keys.ID) error {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			data, err := r.get(id)
			if err != nil {
				return err
			}
			_, err = f.Write(data)
			return err
		})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
