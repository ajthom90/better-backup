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
	// Images are volumes or disks to copy block by block.
	Images []ImageSource
	// VMs are virtual machines: a configuration and disk images each.
	VMs []VMSource
	// Machine, if set, backs up the whole machine for bare-metal recovery.
	Machine *MachineSource
	// SystemState, if set, backs up the files a Windows system state
	// consists of.
	SystemState *SystemState
	// Folders are stored under a name of their own instead of their path.
	Folders []Folder
	// Virtual are sources that do not come from a file system.
	Virtual []VirtualSource
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

// Folder is a folder to back up as a source called Name, whatever its
// path. It is read as it is, not through MapPath.
type Folder struct{ Name, Path string }

// SystemState names the files that make up a system state: everything
// under Roots that Match accepts. Match is asked about folders too, and
// a folder it rejects is not entered.
type SystemState struct {
	Roots []string // drive roots, `C:\`
	Match func(path string, dir bool) bool
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

	// only, while set, limits the walk to what it accepts.
	only func(path string, dir bool) bool

	lastProgress time.Time
}

// Run backs up opts.Paths and returns the snapshot's source entries.
func Run(ctx context.Context, store Store, opts Options) ([]tree.Entry, tree.Stats, error) {
	a := &archiver{ctx: ctx, store: store, opts: opts, inBatch: map[keys.ID]struct{}{}, known: map[keys.ID]struct{}{}}
	if a.opts.MapPath == nil {
		a.opts.MapPath = func(p string) string { return p }
	}
	if len(opts.Paths) == 0 && len(opts.Images) == 0 && len(opts.VMs) == 0 && len(opts.Folders) == 0 && len(opts.Virtual) == 0 && opts.SystemState == nil && opts.Machine == nil {
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
	if opts.SystemState != nil {
		e, err := a.systemState(opts.SystemState, parents[tree.SystemStateName])
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			sources = append(sources, *e)
		}
	}
	if opts.Machine != nil {
		e, err := a.machine(opts.Machine, parents[tree.MachineName])
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			sources = append(sources, *e)
		}
	}
	for _, f := range opts.Folders {
		fi, err := os.Lstat(f.Path)
		if err != nil {
			a.warn("%v", err)
			continue
		}
		mapped := a.opts.MapPath
		a.opts.MapPath = func(p string) string { return p }
		e, err := a.node(f.Path, f.Name, fi, parents[f.Name])
		a.opts.MapPath = mapped
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			sources = append(sources, *e)
		}
	}
	for i := range opts.Images {
		e, err := a.image(&opts.Images[i], parents[opts.Images[i].Name])
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			sources = append(sources, *e)
		}
	}
	for i := range opts.VMs {
		e, err := a.vm(&opts.VMs[i], parents[opts.VMs[i].Name])
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			sources = append(sources, *e)
		}
	}
	for i := range opts.Virtual {
		v := &opts.Virtual[i]
		e, err := a.vdir(v.Root, v.Name, parents[v.Name])
		if err != nil {
			return nil, a.stats, err
		}
		if e != nil {
			e.Name = v.Name
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
		return e, a.streams(path, e)
	case fi.Mode().IsRegular():
		e.Type = tree.TypeFile
		a.stats.Files++
		if parent != nil && parent.Type == tree.TypeFile && parent.Size == fi.Size() && parent.ModTime == e.ModTime {
			// Writing to a stream also updates the file's modification
			// time, so unchanged files have unchanged streams.
			e.Size, e.Content, e.ContentIndex, e.Streams = parent.Size, parent.Content, parent.ContentIndex, parent.Streams
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
		if parent == nil || parent.Type != tree.TypeFile {
			a.stats.FilesNew++
		}
		a.stats.BytesChanged += e.Size
		a.stats.Bytes += e.Size
		a.progress()
		return e, a.streams(path, e)
	}
	return nil, nil // sockets, devices, pipes
}

func (a *archiver) file(path string, e *tree.Entry) (bool, error) {
	ids, size, ok, err := a.chunks(path)
	if err != nil || !ok {
		return false, err
	}
	e.Size = size
	return true, tree.SetContent(e, ids, a.put)
}

// chunks reads path (a file, or a file's named stream) and queues its
// chunks. ok is false when it could not be read; that is a warning, not
// an error.
func (a *archiver) chunks(path string) (ids []keys.ID, size int64, ok bool, err error) {
	f, err := os.Open(a.opts.MapPath(path))
	if err != nil {
		a.warn("%v", err)
		return nil, 0, false, nil
	}
	defer f.Close()
	return a.chunkReader(f, path)
}

// chunkReader is chunks for data that is already open.
func (a *archiver) chunkReader(f io.Reader, path string) (ids []keys.ID, size int64, ok bool, err error) {
	c := chunker.New(f)
	defer c.Release()
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			return ids, size, true, nil
		}
		if err != nil {
			a.warn("read %s: %v", path, err)
			return nil, 0, false, nil
		}
		id, err := a.put(chunk)
		if err != nil {
			return nil, 0, false, err
		}
		ids = append(ids, id)
		size += int64(len(chunk))
		a.stats.BytesRead += int64(len(chunk))
		a.progress()
		if err := a.ctx.Err(); err != nil {
			return nil, 0, false, err
		}
	}
}

// streams backs up the NTFS alternate data streams of a file or
// directory. It does nothing on other systems.
func (a *archiver) streams(path string, e *tree.Entry) error {
	names, err := listStreams(a.opts.MapPath(path))
	if err != nil {
		a.warn("%s: listing alternate data streams: %v", path, err)
		return nil
	}
	for _, name := range names {
		ids, size, ok, err := a.chunks(path + ":" + name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		st := tree.Stream{Name: name, Size: size}
		if st.Content, st.ContentIndex, err = tree.PackIDs(ids, a.put); err != nil {
			return err
		}
		e.Streams = append(e.Streams, st)
	}
	return nil
}

// systemState archives the selected files of each drive under one
// entry, a folder per drive.
func (a *archiver) systemState(src *SystemState, parent *tree.Entry) (*tree.Entry, error) {
	prev := map[string]*tree.Entry{}
	if parent != nil && parent.Type == tree.TypeDir {
		for _, p := range parent.Subtree {
			es, err := tree.ReadPage(a.get, p)
			if err != nil {
				prev = map[string]*tree.Entry{}
				break
			}
			for i := range es {
				prev[es[i].Name] = &es[i]
			}
		}
	}
	a.only = src.Match
	defer func() { a.only = nil }()
	pw := tree.NewPageWriter(a.put)
	before := a.stats.Files
	for _, root := range src.Roots {
		name := strings.TrimRight(filepath.VolumeName(root), ":")
		if name == "" {
			name = filepath.Base(root)
		}
		if name == "" || name == "." || strings.ContainsAny(name, `:\/`) {
			a.warn("system state: skipped %s, which is not a drive", root)
			continue
		}
		fi, err := os.Lstat(a.opts.MapPath(root))
		if err != nil {
			a.warn("system state: %v", err)
			continue
		}
		e, err := a.node(root, name, fi, prev[name])
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
	if a.stats.Files == before {
		a.warn("system state: none of its files could be read")
		return nil, nil
	}
	pages, err := pw.Finish()
	if err != nil {
		return nil, err
	}
	a.stats.Dirs++
	return &tree.Entry{Name: tree.SystemStateName, Type: tree.TypeDir, Mode: 0o755, ModTime: time.Now().UnixNano(), Subtree: pages}, nil
}

// prevEntries returns what a folder held in the last backup, by name.
func (a *archiver) prevEntries(parent *tree.Entry) map[string]*tree.Entry {
	prev := map[string]*tree.Entry{}
	if parent != nil && parent.Type == tree.TypeDir {
		for _, p := range parent.Subtree {
			es, err := tree.ReadPage(a.get, p)
			if err != nil {
				// Without the previous listing every file is simply
				// read again; the backup is still correct.
				return map[string]*tree.Entry{}
			}
			for i := range es {
				prev[es[i].Name] = &es[i]
			}
		}
	}
	return prev
}

func (a *archiver) dir(path string, parent *tree.Entry) ([]keys.ID, error) {
	prev := a.prevEntries(parent)
	items, err := os.ReadDir(a.opts.MapPath(path))
	if err != nil {
		a.warn("%v", err)
		if len(items) == 0 {
			return nil, nil
		}
	}
	pw := tree.NewPageWriter(a.put)
	// What the backup before had here and this one does not is removed.
	if parent != nil && err == nil {
		here := make(map[string]struct{}, len(items))
		for _, d := range items {
			here[d.Name()] = struct{}{}
		}
		for name := range prev {
			if _, ok := here[name]; !ok {
				a.stats.FilesRemoved++
			}
		}
	}
	for _, d := range items {
		child := filepath.Join(path, d.Name())
		if a.excluded(child) {
			continue
		}
		if a.only != nil && !a.only(child, d.IsDir()) {
			continue
		}
		fi, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				a.warn("%v", err)
			}
			continue
		}
		if mapped := a.opts.MapPath(child); mapped != filepath.Join(a.opts.MapPath(path), d.Name()) {
			// The child is read from somewhere else than this folder:
			// it is where another dataset is mounted. What the folder
			// lists is the empty mount point; the real one has its own
			// owner and permissions.
			if real, err := os.Lstat(mapped); err == nil {
				fi = real
			}
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
