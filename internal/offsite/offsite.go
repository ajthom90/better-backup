// Package offsite keeps a second copy of a repository somewhere else: a
// folder on another disk or a network share, or a bucket in S3-compatible
// object storage.
//
// A repository is made of files that never change once written (pack
// files and snapshot records), so a copy is kept current by uploading the
// files it lacks and removing the ones the repository dropped. Everything
// in those files is already encrypted. The copy has the same layout as the
// repository, so once it is downloaded to a folder it can be opened with
// the repository key like the original.
package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Object is one file of a copy. Names use forward slashes.
type Object struct {
	Name string
	Size int64
}

// Target is where a copy lives.
type Target interface {
	// List returns every object of the copy.
	List(ctx context.Context) ([]Object, error)
	// Put stores size bytes from r under name, replacing what is there.
	Put(ctx context.Context, name string, r io.ReadSeeker, size int64) error
	// Get opens an object. A missing object gives fs.ErrNotExist.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	Delete(ctx context.Context, name string) error
	// String says where the copy is, for messages.
	String() string
}

// Result reports what a Sync did.
type Result struct {
	Uploaded      int   `json:"uploaded"`
	UploadedBytes int64 `json:"uploaded_bytes"`
	Deleted       int   `json:"deleted"`
	// Objects and Bytes describe the copy after the run.
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

const configName = "config.json"

// stateName is the server's own configuration, saved into the
// repository so that a copy can bring back a whole server. It changes
// in place, so its size says nothing about whether the copy has it.
const stateName = "state/server.bak"

// wanted reports whether a repository file belongs in a copy. The index
// is a cache and temporary files are unfinished.
func wanted(name string) bool {
	if strings.Contains(name, ".tmp-") {
		return false
	}
	switch {
	case name == configName:
		return true
	case strings.HasPrefix(name, "packs/") && strings.HasSuffix(name, ".pack"):
		return true
	case strings.HasPrefix(name, "snapshots/") && strings.HasSuffix(name, ".snap"):
		return true
	case name == stateName:
		return true
	}
	return false
}

func local(dir, sub string) ([]Object, error) {
	var out []Object
	root := filepath.Join(dir, sub)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		name := filepath.ToSlash(rel)
		if !wanted(name) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, Object{name, fi.Size()})
		return nil
	})
	return out, err
}

func repoID(b []byte) string {
	var c struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &c)
	return c.ID
}

// Sync brings the copy at t up to date with the repository in dir.
//
// It may run while backups write to the repository: what they add is
// picked up by the next run. It must not run while the repository is
// being garbage collected, because that moves data between pack files.
//
// progress, if set, is called with the running totals.
func Sync(ctx context.Context, dir string, t Target, progress func(Result)) (Result, error) {
	var res Result
	cfg, err := os.ReadFile(filepath.Join(dir, configName))
	if err != nil {
		return res, fmt.Errorf("reading the repository: %w", err)
	}
	id := repoID(cfg)
	if id == "" {
		return res, errors.New("the repository's config.json has no id")
	}
	// Never mix two repositories in one place.
	if rc, err := t.Get(ctx, configName); err == nil {
		b, rerr := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if rerr != nil {
			return res, fmt.Errorf("reading the copy's config.json: %w", rerr)
		}
		if other := repoID(b); other != id {
			return res, fmt.Errorf("%s already holds a copy of a different repository (%s); choose an empty place or another prefix", t, other)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("reading the copy: %w", err)
	}

	// Snapshots are listed before packs and uploaded after them. A
	// snapshot's data is on disk before its record is, so every snapshot
	// in this list has all its packs in the list that follows, and the
	// copy never holds a snapshot it cannot restore.
	snaps, err := local(dir, "snapshots")
	if err != nil {
		return res, err
	}
	packs, err := local(dir, "packs")
	if err != nil {
		return res, err
	}
	remote, err := t.List(ctx)
	if err != nil {
		return res, fmt.Errorf("listing the copy: %w", err)
	}
	have := map[string]int64{}
	for _, o := range remote {
		have[o.Name] = o.Size
	}
	keep := map[string]bool{configName: true}
	upload := func(o Object) error {
		keep[o.Name] = true
		if size, ok := have[o.Name]; ok && size == o.Size {
			res.Objects++
			res.Bytes += o.Size
			return nil
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(o.Name)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				keep[o.Name] = false // forgotten since it was listed
				return nil
			}
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if err := t.Put(ctx, o.Name, f, fi.Size()); err != nil {
			return fmt.Errorf("uploading %s: %w", o.Name, err)
		}
		res.Uploaded++
		res.UploadedBytes += fi.Size()
		res.Objects++
		res.Bytes += fi.Size()
		if progress != nil {
			progress(res)
		}
		return ctx.Err()
	}
	for _, o := range packs {
		if err := upload(o); err != nil {
			return res, err
		}
	}
	for _, o := range snaps {
		if err := upload(o); err != nil {
			return res, err
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(stateName))); err == nil {
		// Always sent again, and not counted as news: it is there to
		// bring a server back, not something the user backed up.
		delete(have, stateName)
		n, b := res.Uploaded, res.UploadedBytes
		if err := upload(Object{stateName, fi.Size()}); err != nil {
			return res, err
		}
		res.Uploaded, res.UploadedBytes = n, b
	}
	// The config goes last: its presence marks a copy that can be used.
	if err := t.Put(ctx, configName, bytes.NewReader(cfg), int64(len(cfg))); err != nil {
		return res, fmt.Errorf("uploading %s: %w", configName, err)
	}
	res.Objects++
	res.Bytes += int64(len(cfg))

	// Remove what the repository no longer has: forgotten snapshots
	// first, then the packs garbage collection dropped.
	var extra []string
	for _, o := range remote {
		if wanted(o.Name) && !keep[o.Name] {
			extra = append(extra, o.Name)
		}
	}
	// A repository that suddenly lost most of its files is more likely
	// a failed disk or a missing mount than a clean-up. Keep the copy.
	if n := len(remote); len(extra) > 20 && len(extra)*2 > n {
		return res, fmt.Errorf("the repository no longer has %d of the %d files in the copy; nothing was removed from the copy in case the repository is damaged", len(extra), n)
	}
	sort.Slice(extra, func(i, j int) bool {
		si, sj := strings.HasPrefix(extra[i], "snapshots/"), strings.HasPrefix(extra[j], "snapshots/")
		if si != sj {
			return si
		}
		return extra[i] < extra[j]
	})
	for _, name := range extra {
		if err := t.Delete(ctx, name); err != nil {
			return res, fmt.Errorf("removing %s from the copy: %w", name, err)
		}
		res.Deleted++
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	if progress != nil {
		progress(res)
	}
	return res, nil
}

// Fetch downloads the copy at t into dir, which must not already hold a
// repository. Files already present with the right size are kept, so an
// interrupted download can be resumed.
func Fetch(ctx context.Context, t Target, dir string, progress func(done, total int64)) error {
	remote, err := t.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the copy: %w", err)
	}
	var total, done int64
	found := false
	for _, o := range remote {
		if wanted(o.Name) {
			total += o.Size
			found = found || o.Name == configName
		}
	}
	if !found {
		return fmt.Errorf("%s holds no complete copy of a repository (no config.json)", t)
	}
	// config.json last, as in Sync.
	sort.SliceStable(remote, func(i, j int) bool { return remote[i].Name != configName && remote[j].Name == configName })
	for _, o := range remote {
		if !wanted(o.Name) || strings.Contains(o.Name, "..") {
			continue
		}
		dest := filepath.Join(dir, filepath.FromSlash(o.Name))
		if fi, err := os.Stat(dest); err == nil && fi.Size() == o.Size && o.Name != configName {
			done += o.Size
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if err := fetchOne(ctx, t, o, dest); err != nil {
			return fmt.Errorf("downloading %s: %w", o.Name, err)
		}
		done += o.Size
		if progress != nil {
			progress(done, total)
		}
	}
	for _, d := range []string{"packs", "snapshots", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func fetchOne(ctx context.Context, t Target, o Object, dest string) error {
	rc, err := t.Get(ctx, o.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dest + ".tmp-fetch"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, rc)
	if err == nil && n != o.Size {
		err = fmt.Errorf("got %d bytes, expected %d", n, o.Size)
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
