// Package backup walks file-system paths and stores them as a snapshot
// tree, uploading only the chunks the store does not already have.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ajthom90/better-backup/internal/chunker"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// Store is where chunks go: the server, seen from an agent.
type Store interface {
	Missing(ctx context.Context, ids []keys.ID) ([]keys.ID, error)
	Put(ctx context.Context, id keys.ID, data []byte) error
	Get(ctx context.Context, id keys.ID) ([]byte, error)
}

// Options configures one backup run.
type Options struct {
	Paths    []string
	Excludes []string
	// Parent holds the sources of the previous snapshot. Files whose size
	// and modification time match it are not read again.
	Parent []tree.Entry
	IDKey  [32]byte
	// MapPath, if set, turns a path into the path to read it from. It is
	// how a backup reads from a volume snapshot while recording the
	// original paths.
	MapPath  func(string) string
	Progress func(tree.Stats)
	Warn     func(string)
}

const (
	batchBytes  = 64 << 20
	batchChunks = 1024
	uploaders   = 4
	knownLimit  = 1 << 20
)

type item struct {
	id   keys.ID
	data []byte
}

type archiver struct {
	ctx   context.Context
	store Store
	opts  Options
	stats tree.Stats

	batch    []item
	inBatch  map[keys.ID]struct{}
	batchLen int
	known    map[keys.ID]struct{}

	lastProgress time.Time
}

// Run backs up opts.Paths and returns the snapshot's source entries.
func Run(ctx context.Context, store Store, opts Options) ([]tree.Entry, tree.Stats, error) {
	a := &archiver{ctx: ctx, store: store, opts: opts, inBatch: map[keys.ID]struct{}{}, known: map[keys.ID]struct{}{}}
	if a.opts.MapPath == nil {
		a.opts.MapPath = func(p string) string { return p }
	}
	if len(opts.Paths) == 0 {
		return nil, a.stats, errors.New("no paths to back up")
	}
	parents := map[string]*tree.Entry{}
	for i := range opts.Parent {
		parents[opts.Parent[i].Name] = &opts.Parent[i]
	}
	var sources []tree.Entry
	for _, p := range opts.Paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, a.stats, err
		}
		fi, err := os.Lstat(a.opts.MapPath(abs))
		if err != nil {
			a.warn("%v", err)
			continue
		}
		e, err := a.node(abs, abs, fi, parents[abs])
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			sources = append(sources, *e)
		}
	}
	if len(sources) == 0 {
		return nil, a.stats, errors.New("none of the configured paths could be read")
	}
	if err := a.flush(); err != nil {
		return nil, a.stats, err
	}
	return sources, a.stats, nil
}

func (a *archiver) warn(format string, args ...any) {
	a.stats.Warnings++
	if a.opts.Warn != nil {
		a.opts.Warn(fmt.Sprintf(format, args...))
	}
}

func (a *archiver) progress() {
	if a.opts.Progress != nil && time.Since(a.lastProgress) > 2*time.Second {
		a.lastProgress = time.Now()
		a.opts.Progress(a.stats)
	}
}

// put queues a chunk for upload and returns its id.
func (a *archiver) put(data []byte) (keys.ID, error) {
	id := keys.ChunkID(&a.opts.IDKey, data)
	if _, ok := a.known[id]; ok {
		return id, nil
	}
	if _, ok := a.inBatch[id]; ok {
		return id, nil
	}
	a.batch = append(a.batch, item{id, append([]byte(nil), data...)})
	a.inBatch[id] = struct{}{}
	a.batchLen += len(data)
	if a.batchLen >= batchBytes || len(a.batch) >= batchChunks {
		return id, a.flush()
	}
	return id, nil
}

// flush asks the store which queued chunks it lacks and uploads those.
func (a *archiver) flush() error {
	if len(a.batch) == 0 {
		return nil
	}
	ids := make([]keys.ID, len(a.batch))
	for i, it := range a.batch {
		ids[i] = it.id
	}
	missing, err := a.store.Missing(a.ctx, ids)
	if err != nil {
		return err
	}
	need := make(map[keys.ID]struct{}, len(missing))
	for _, id := range missing {
		need[id] = struct{}{}
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		work  = make(chan item)
	)
	for i := 0; i < uploaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				if err := a.store.Put(ctx, it.id, it.data); err != nil {
					once.Do(func() { first = err; cancel() })
				}
			}
		}()
	}
	for _, it := range a.batch {
		if _, ok := need[it.id]; !ok {
			continue
		}
		a.stats.ChunksNew++
		a.stats.BytesUploaded += int64(len(it.data))
		select {
		case work <- it:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	if first != nil {
		return first
	}
	if err := a.ctx.Err(); err != nil {
		return err
	}
	if len(a.known) > knownLimit {
		a.known = map[keys.ID]struct{}{}
	}
	for _, id := range ids {
		a.known[id] = struct{}{}
	}
	a.batch, a.batchLen = a.batch[:0], 0
	a.inBatch = map[keys.ID]struct{}{}
	return nil
}

func (a *archiver) get(id keys.ID) ([]byte, error) { return a.store.Get(a.ctx, id) }

func (a *archiver) excluded(path string) bool {
	if runtime.GOOS == "linux" {
		for _, p := range []string{"/proc", "/sys", "/dev", "/run"} {
			if path == p {
				return true
			}
		}
	}
	base := filepath.Base(path)
	for _, pat := range a.opts.Excludes {
		if pat == path || strings.HasPrefix(path, strings.TrimRight(pat, `/\`)+string(filepath.Separator)) {
			return true
		}
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, path); ok {
			return true
		}
	}
	return false
}

// node archives one file-system object. path is its real location; all
// reads go through MapPath. It returns nil for objects that
// are skipped. Only store errors are returned; unreadable files become
// warnings so one bad file does not fail the backup.
func (a *archiver) node(path, name string, fi fs.FileInfo, parent *tree.Entry) (*tree.Entry, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	e := &tree.Entry{Name: name, ModTime: fi.ModTime().UnixNano()}
	if err := fillMeta(e, a.opts.MapPath(path), fi); err != nil {
		a.warn("%s: reading permissions: %v", path, err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(a.opts.MapPath(path))
		if err != nil {
			a.warn("%v", err)
			return nil, nil
		}
		e.Type, e.LinkTarget = tree.TypeSymlink, target
		return e, nil
	case fi.IsDir():
		e.Type = tree.TypeDir
		pages, err := a.dir(path, parent)
		if err != nil {
			return nil, err
		}
		a.stats.Dirs++
		e.Subtree = pages
		return e, nil
	case fi.Mode().IsRegular():
		e.Type = tree.TypeFile
		a.stats.Files++
		if parent != nil && parent.Type == tree.TypeFile && parent.Size == fi.Size() && parent.ModTime == e.ModTime {
			e.Size, e.Content, e.ContentIndex = parent.Size, parent.Content, parent.ContentIndex
			a.stats.Bytes += e.Size
			a.progress()
			return e, nil
		}
		ok, err := a.file(path, e)
		if err != nil || !ok {
			a.stats.Files--
			return nil, err
		}
		a.stats.FilesChanged++
		a.stats.Bytes += e.Size
		a.progress()
		return e, nil
	}
	return nil, nil // sockets, devices, pipes
}

func (a *archiver) file(path string, e *tree.Entry) (bool, error) {
	f, err := os.Open(a.opts.MapPath(path))
	if err != nil {
		a.warn("%v", err)
		return false, nil
	}
	defer f.Close()
	var ids []keys.ID
	var size int64
	c := chunker.New(f)
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			a.warn("read %s: %v", path, err)
			return false, nil
		}
		id, err := a.put(chunk)
		if err != nil {
			return false, err
		}
		ids = append(ids, id)
		size += int64(len(chunk))
		a.stats.BytesRead += int64(len(chunk))
		a.progress()
		if err := a.ctx.Err(); err != nil {
			return false, err
		}
	}
	e.Size = size
	return true, tree.SetContent(e, ids, a.put)
}

func (a *archiver) dir(path string, parent *tree.Entry) ([]keys.ID, error) {
	prev := map[string]*tree.Entry{}
	if parent != nil && parent.Type == tree.TypeDir {
		for _, p := range parent.Subtree {
			es, err := tree.ReadPage(a.get, p)
			if err != nil {
				// Without the previous listing every file is simply
				// read again; the backup is still correct.
				prev = map[string]*tree.Entry{}
				break
			}
			for i := range es {
				prev[es[i].Name] = &es[i]
			}
		}
	}
	items, err := os.ReadDir(a.opts.MapPath(path))
	if err != nil {
		a.warn("%v", err)
		if len(items) == 0 {
			return nil, nil
		}
	}
	pw := tree.NewPageWriter(a.put)
	for _, d := range items {
		child := filepath.Join(path, d.Name())
		if a.excluded(child) {
			continue
		}
		fi, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				a.warn("%v", err)
			}
			continue
		}
		e, err := a.node(child, d.Name(), fi, prev[d.Name()])
		if err != nil {
			return nil, err
		}
		if e == nil {
			continue
		}
		if err := pw.Add(e); err != nil {
			return nil, err
		}
	}
	return pw.Finish()
}
