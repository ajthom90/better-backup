package backup

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/ajthom90/better-backup/internal/tree"
)

// VirtualSource is a tree of folders and files that does not come from
// a file system: the mailboxes and drives of a cloud service. It is
// stored as a source called Name.
type VirtualSource struct {
	Name string
	Root *VNode
}

// VNode is one folder or file of a VirtualSource.
type VNode struct {
	// Name must be usable as a file name on every system, and be the
	// same from one backup to the next.
	Name    string
	Dir     bool
	ModTime time.Time
	// Key names this version of a file. While it stays the same as in
	// the last backup the file is not opened. Empty: always opened.
	Key string
	// List returns what a folder holds.
	List func(ctx context.Context) ([]*VNode, error)
	// Open returns the content of a file.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

const (
	// fetchers is how many files of one folder are downloaded at once,
	// fetchAhead how many downloads may wait to be stored, and fetchHead
	// how much of each of those is held in memory; the rest of a larger
	// file is read when its turn comes.
	fetchers   = 4
	fetchAhead = 8
	fetchHead  = 4 << 20
)

type fetched struct {
	head []byte
	rest io.ReadCloser
	err  error
}

func fetch(ctx context.Context, n *VNode) fetched {
	rc, err := n.Open(ctx)
	if err != nil {
		return fetched{err: err}
	}
	head, err := io.ReadAll(io.LimitReader(rc, fetchHead))
	if err != nil {
		rc.Close()
		return fetched{err: err}
	}
	if len(head) < fetchHead {
		rc.Close()
		return fetched{head: head}
	}
	return fetched{head: head, rest: rc}
}

// vdir archives a folder of a virtual source. When the folder cannot be
// listed, or a file cannot be read, what the last backup held of it is
// kept, so a service that fails for a moment does not make its data
// vanish from the newest snapshot.
func (a *archiver) vdir(n *VNode, path string, parent *tree.Entry) (*tree.Entry, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	kids, err := n.List(a.ctx)
	if err != nil {
		if cerr := a.ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if parent != nil && parent.Type == tree.TypeDir {
			a.warn("%s: %v; kept as it was in the last backup", path, err)
			keep := *parent
			return &keep, nil
		}
		a.warn("%s: %v", path, err)
		return nil, nil
	}
	sort.SliceStable(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
	prev := a.prevEntries(parent)

	// Files that changed are downloaded a few at a time, ahead of the
	// place the walk has reached.
	ctx, cancel := context.WithCancel(a.ctx)
	results := make([]chan fetched, len(kids))
	var need []int
	seen := map[string]bool{}
	for i, k := range kids {
		if seen[k.Name] || k.Name == "" {
			a.warn("%s: skipped a second item called %q", path, k.Name)
			kids[i] = nil
			continue
		}
		seen[k.Name] = true
		if k.Dir {
			continue
		}
		if p := prev[k.Name]; p != nil && p.Type == tree.TypeFile && k.Key != "" && p.Key == k.Key {
			continue
		}
		results[i] = make(chan fetched, 1)
		need = append(need, i)
	}
	var wg sync.WaitGroup
	work, room := make(chan int), make(chan struct{}, fetchAhead)
	if len(need) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(work)
			for _, i := range need {
				select {
				case room <- struct{}{}:
				case <-ctx.Done():
					return
				}
				select {
				case work <- i:
				case <-ctx.Done():
					return
				}
			}
		}()
		for w := 0; w < min(fetchers, len(need)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					results[i] <- fetch(ctx, kids[i])
				}
			}()
		}
	}
	defer func() {
		cancel()
		wg.Wait()
		for _, ch := range results {
			if ch == nil {
				continue
			}
			select {
			case f := <-ch:
				if f.rest != nil {
					f.rest.Close()
				}
			default:
			}
		}
	}()

	pw := tree.NewPageWriter(a.put)
	for i, k := range kids {
		if k == nil {
			continue
		}
		child := path + "/" + k.Name
		old := prev[k.Name]
		var e *tree.Entry
		switch {
		case k.Dir:
			if e, err = a.vdir(k, child, old); err != nil {
				return nil, err
			}
			if e != nil {
				e.Name = k.Name
			}
		case results[i] == nil:
			keep := *old
			keep.ModTime = k.ModTime.UnixNano()
			e = &keep
			a.stats.Files++
			a.stats.Bytes += e.Size
		default:
			var f fetched
			select {
			case f = <-results[i]:
				results[i] = nil
				<-room
			case <-a.ctx.Done():
				return nil, a.ctx.Err()
			}
			if e, err = a.vfile(k, child, f, old); err != nil {
				return nil, err
			}
		}
		a.progress()
		if e == nil {
			continue
		}
		if err := pw.Add(e); err != nil {
			return nil, err
		}
	}
	pages, err := pw.Finish()
	if err != nil {
		return nil, err
	}
	a.stats.Dirs++
	e := &tree.Entry{Name: n.Name, Type: tree.TypeDir, Mode: 0o755, ModTime: n.ModTime.UnixNano(), Subtree: pages}
	if n.ModTime.IsZero() {
		e.ModTime = 0
		if parent != nil {
			e.ModTime = parent.ModTime
		}
		if e.ModTime == 0 {
			e.ModTime = time.Now().UnixNano()
		}
	}
	return e, nil
}

// vfile stores a downloaded file.
func (a *archiver) vfile(k *VNode, path string, f fetched, old *tree.Entry) (*tree.Entry, error) {
	if f.rest != nil {
		defer f.rest.Close()
	}
	stale := func(err error) (*tree.Entry, error) {
		if cerr := a.ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if old != nil && old.Type == tree.TypeFile {
			a.warn("%s: %v; kept as it was in the last backup", path, err)
			keep := *old
			a.stats.Files++
			a.stats.Bytes += keep.Size
			return &keep, nil
		}
		a.warn("%s: %v", path, err)
		return nil, nil
	}
	if f.err != nil {
		return stale(f.err)
	}
	var r io.Reader = bytes.NewReader(f.head)
	if f.rest != nil {
		r = io.MultiReader(r, f.rest)
	}
	ids, size, ok, err := a.chunkReader(r, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		// chunkReader has said why. What the last backup held stays.
		if old != nil && old.Type == tree.TypeFile {
			keep := *old
			a.stats.Files++
			a.stats.Bytes += keep.Size
			return &keep, nil
		}
		return nil, nil
	}
	e := &tree.Entry{Name: k.Name, Type: tree.TypeFile, Mode: 0o644, ModTime: k.ModTime.UnixNano(), Size: size, Key: k.Key}
	if err := tree.SetContent(e, ids, a.put); err != nil {
		return nil, err
	}
	a.stats.Files++
	a.stats.FilesChanged++
	a.stats.Bytes += size
	return e, nil
}
