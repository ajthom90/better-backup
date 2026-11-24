package server

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// download is a link to one file or folder of a snapshot. A browser
// cannot send a session with a plain download, so a signed-in operator
// asks for a link, and the link itself is the permission: it is long,
// random, works once and for two minutes.
type download struct {
	snapshot string
	path     string
	expires  time.Time
}

const downloadLinkLife = 2 * time.Minute

var errGC = errors.New("the repository is being cleaned up just now; try again when “Reclaim space” has finished")

// beginRead says the repository is being read outside a job, so that
// garbage collection does not remove packs under the reader. The
// function it returns ends the reading.
func (s *Server) beginRead() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gcRunning {
		return nil, errGC
	}
	s.reading++
	return func() {
		s.mu.Lock()
		s.reading--
		s.mu.Unlock()
		s.kickBackground()
	}, nil
}

// downloadName is the file name a download is saved under.
func downloadName(e *tree.Entry) string {
	name := e.Name
	if i := strings.LastIndexAny(strings.TrimRight(name, `/\`), `/\`); i >= 0 {
		name = strings.TrimRight(name, `/\`)[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "backup"
	}
	switch e.Type {
	case tree.TypeDir, tree.TypeVM, tree.TypeMachine:
		name += ".zip"
	case tree.TypeImage:
		if !strings.Contains(name, ".") {
			name += ".img"
		}
	}
	return name
}

func (s *Server) handleDownloadLink(w http.ResponseWriter, r *http.Request) {
	var req api.DownloadRequest
	if !decode(w, r, &req) {
		return
	}
	sn := s.snapshot(r.PathValue("id"))
	if sn == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	if req.Path == "" {
		fail(w, http.StatusBadRequest, "say which file or folder of the snapshot to download")
		return
	}
	done, err := s.beginRead()
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	e, err := findEntry(s.repo.Get, sn, req.Path)
	done()
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if e.Type == tree.TypeSymlink {
		fail(w, http.StatusBadRequest, "this is a link to "+e.LinkTarget+", not a file; download what it points at")
		return
	}
	ticket := keys.RandomHex(24)
	s.mu.Lock()
	if s.downloads == nil {
		s.downloads = map[string]*download{}
	}
	for k, d := range s.downloads {
		if time.Now().After(d.expires) {
			delete(s.downloads, k)
		}
	}
	s.downloads[ticket] = &download{snapshot: sn.ID, path: req.Path, expires: time.Now().Add(downloadLinkLife)}
	s.mu.Unlock()
	note(r, "%s from %s, %s", req.Path, sn.Hostname, sn.Time.Local().Format("2006-01-02 15:04"))
	out := api.DownloadLink{URL: "/api/v1/downloads/" + ticket, Name: downloadName(e)}
	if e.Type == tree.TypeFile || e.Type == tree.TypeImage {
		out.Size = e.Size
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	ticket := r.PathValue("ticket")
	s.mu.Lock()
	d := s.downloads[ticket]
	delete(s.downloads, ticket)
	s.mu.Unlock()
	if d == nil || time.Now().After(d.expires) {
		http.Error(w, "This download link has been used or has run out. Go back to the snapshot and choose Download again.", http.StatusGone)
		return
	}
	sn := s.snapshot(d.snapshot)
	if sn == nil {
		http.Error(w, "The snapshot is gone.", http.StatusNotFound)
		return
	}
	done, err := s.beginRead()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer done()
	e, err := findEntry(s.repo.Get, sn, d.path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// The repository's reads do not stop for a browser that went
	// away, so look between chunks.
	ctx := r.Context()
	get := func(id keys.ID) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return s.repo.Get(id)
	}
	h := w.Header()
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": downloadName(e)}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if isFolder(e) {
		h.Set("Content-Type", "application/zip")
		zw := zip.NewWriter(w)
		root := strings.TrimSuffix(downloadName(e), ".zip")
		if err := zipEntry(get, zw, e, root); err != nil {
			// The answer is under way; all that can be done is to cut it
			// short, so that the browser does not take it for complete.
			s.log.Warn("download stopped", "path", d.path, "err", err)
			panic(http.ErrAbortHandler)
		}
		if err := zw.Close(); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(e.Size, 10))
	if err := writeContent(get, w, e); err != nil {
		s.log.Warn("download stopped", "path", d.path, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// writeContent writes the data of a file, or of an image as a raw disk
// image whose holes are zeros.
func writeContent(get tree.Getter, w io.Writer, e *tree.Entry) error {
	if e.Type != tree.TypeImage {
		return tree.EachContent(get, e, func(id keys.ID) error {
			data, err := get(id)
			if err != nil {
				return err
			}
			_, err = w.Write(data)
			return err
		})
	}
	if e.BlockSize <= 0 {
		return fmt.Errorf("image %s has no block size", e.Name)
	}
	off := int64(0)
	var zeros []byte
	err := tree.EachContent(get, e, func(id keys.ID) error {
		n := min(e.BlockSize, e.Size-off)
		if n <= 0 {
			return fmt.Errorf("image %s lists more blocks than its size", e.Name)
		}
		data := zeros
		if id != (keys.ID{}) {
			var err error
			if data, err = get(id); err != nil {
				return err
			}
			if int64(len(data)) != n {
				return fmt.Errorf("image %s: block at offset %d has %d bytes, want %d", e.Name, off, len(data), n)
			}
		} else {
			if zeros == nil {
				zeros = make([]byte, e.BlockSize)
			}
			data = zeros[:n]
		}
		off += n
		_, err := w.Write(data)
		return err
	})
	if err == nil && off != e.Size {
		err = fmt.Errorf("image %s: wrote %d bytes but the backup recorded %d", e.Name, off, e.Size)
	}
	return err
}

// zipEntry adds e, and everything in it, to a zip file under name.
func zipEntry(get tree.Getter, zw *zip.Writer, e *tree.Entry, name string) error {
	mod := time.Unix(0, e.ModTime)
	switch {
	case isFolder(e):
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Modified: mod}); err != nil {
			return err
		}
		kids, err := children(get, e)
		if err != nil {
			return err
		}
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
		for i := range kids {
			k := &kids[i]
			if k.Name == "" || k.Name == "." || k.Name == ".." || strings.ContainsAny(k.Name, `/\`) {
				continue // never a path that climbs out of the folder
			}
			if err := zipEntry(get, zw, k, path.Join(name, k.Name)); err != nil {
				return err
			}
		}
	case e.Type == tree.TypeFile || e.Type == tree.TypeImage:
		// Data that is already compressed gains nothing from a second
		// pass; small text does. Deflate is cheap enough for both.
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Modified: mod, Method: zip.Deflate})
		if err != nil {
			return err
		}
		return writeContent(get, f, e)
	}
	return nil // links are left out
}

// Searching.

const (
	searchLimit = 200
	searchTime  = 25 * time.Second
)

// found is a match below some folder: the path from that folder down.
type found struct {
	rel   []string
	entry *tree.Entry
}

type searcher struct {
	ctx   context.Context
	get   tree.Getter
	match func(name string) bool
	// pages remembers what each directory page holds below it. Most
	// pages are shared between snapshots, so each is read once.
	pages map[keys.ID][]found
	cut   bool // stopped early: out of time, or enough found
}

// page returns the matches in one directory page and below it.
func (s *searcher) page(id keys.ID) ([]found, error) {
	if out, ok := s.pages[id]; ok {
		return out, nil
	}
	if s.ctx.Err() != nil {
		s.cut = true
		return nil, nil
	}
	es, err := tree.ReadPage(s.get, id)
	if err != nil {
		return nil, err
	}
	var out []found
	for i := range es {
		e := &es[i]
		if s.match(e.Name) {
			slim := *e
			slim.Content, slim.ContentIndex, slim.Subtree, slim.WinSD, slim.Xattrs, slim.Streams = nil, nil, nil, nil, nil, nil
			out = append(out, found{rel: []string{e.Name}, entry: &slim})
		}
		if e.Type != tree.TypeDir {
			continue
		}
		for _, p := range e.Subtree {
			sub, err := s.page(p)
			if err != nil {
				return nil, err
			}
			for _, f := range sub {
				out = append(out, found{rel: append([]string{e.Name}, f.rel...), entry: f.entry})
			}
		}
		if len(out) > 20*searchLimit {
			s.cut = true
			break
		}
	}
	if !s.cut {
		s.pages[id] = out
	}
	return out, nil
}

// matcher turns what was typed into a test of a file name: a pattern
// if it holds * or ?, otherwise any part of the name. Case is ignored.
func matcher(q string) func(string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	if strings.ContainsAny(q, "*?") {
		return func(name string) bool {
			ok, _ := path.Match(q, strings.ToLower(name))
			return ok
		}
	}
	return func(name string) bool { return strings.Contains(strings.ToLower(name), q) }
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("q"))
	if len([]rune(strings.Trim(text, "*?"))) < 2 {
		fail(w, http.StatusBadRequest, "type at least two letters of the file's name")
		return
	}
	if strings.ContainsAny(text, "*?") {
		if _, err := path.Match(strings.ToLower(text), ""); err != nil {
			fail(w, http.StatusBadRequest, "the pattern is not valid; use * for any letters and ? for one")
			return
		}
	}
	var snaps []*tree.Snapshot
	s.mu.Lock()
	for _, sn := range s.snaps {
		if q.Get("agent_id") == "" || sn.AgentID == q.Get("agent_id") {
			snaps = append(snaps, sn)
		}
	}
	s.mu.Unlock()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	done, err := s.beginRead()
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), searchTime)
	defer cancel()
	se := &searcher{ctx: ctx, get: s.repo.Get, match: matcher(text), pages: map[keys.ID][]found{}}
	out := api.SearchResult{Hits: []api.SearchHit{}}
	// Newest snapshot first, so a file is shown as it last was. Older
	// snapshots add to the count, and bring files that are gone since.
	byPath := map[string]int{}
	for _, sn := range snaps {
		sep := "/"
		if sn.OS == "windows" {
			sep = `\`
		}
		out.Snapshots++
		for i := range sn.Sources {
			src := &sn.Sources[i]
			var fs []found
			if se.match(lastPart(src.Name)) && src.Type != tree.TypeDir {
				fs = append(fs, found{entry: src})
			}
			if src.Type == tree.TypeDir {
				for _, p := range src.Subtree {
					sub, err := se.page(p)
					if err != nil {
						s.internal(w, err)
						return
					}
					fs = append(fs, sub...)
				}
			}
			for _, f := range fs {
				full := src.Name
				if len(f.rel) > 0 {
					full = strings.TrimRight(src.Name, sep) + sep + strings.Join(f.rel, sep)
				}
				key := sn.AgentID + "\x00" + full
				if sn.OS == "windows" {
					key = strings.ToLower(key)
				}
				if at, ok := byPath[key]; ok {
					h := &out.Hits[at]
					h.Snapshots++
					h.Oldest = sn.Time
					continue
				}
				if len(out.Hits) >= searchLimit {
					out.More = true
					continue
				}
				byPath[key] = len(out.Hits)
				out.Hits = append(out.Hits, api.SearchHit{SnapshotID: sn.ID, Time: sn.Time, Oldest: sn.Time, Snapshots: 1, AgentID: sn.AgentID, Hostname: sn.Hostname,
					Path: full, Name: lastPart(full), Type: f.entry.Type, Size: f.entry.Size, ModTime: time.Unix(0, f.entry.ModTime)})
			}
		}
		if se.cut || ctx.Err() != nil {
			out.More, out.Partial = true, ctx.Err() != nil
			break
		}
	}
	reply(w, http.StatusOK, out)
}

func lastPart(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
