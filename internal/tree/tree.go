// Package tree defines how file-system metadata is stored in the
// repository: snapshots point at directory entries, directories are split
// into pages of entries, and files list the chunks that hold their data.
package tree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ajthom90/better-backup/internal/keys"
)

const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	// TypeImage is a block-level copy of a volume or disk. Its content
	// lists one chunk per BlockSize bytes; the zero id stands for a block
	// that was unallocated and holds nothing worth keeping.
	TypeImage = "image"

	// SystemStateName is the name of the source entry that holds a
	// Windows system state: a folder with one folder per drive ("C"),
	// each holding the files the system's VSS writers asked for at their
	// original paths.
	SystemStateName = "System state"

	// pageSize is the target size of one page of directory entries.
	pageSize = 1 << 20
	// inlineContent is the most chunk ids kept inline in a file entry.
	// Larger files store their id list in index chunks instead.
	inlineContent = 1024
	// idsPerIndex is the number of ids in one index chunk (1 MiB).
	idsPerIndex = 32768
)

// Entry describes one file-system object.
type Entry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Mode     uint32 `json:"mode"` // Unix permission and special bits
	Size     int64  `json:"size,omitempty"`
	ModTime  int64  `json:"mtime"` // Unix nanoseconds
	UID      uint32 `json:"uid,omitempty"`
	GID      uint32 `json:"gid,omitempty"`
	WinAttrs uint32 `json:"win_attrs,omitempty"`
	// WinSD is the Windows security descriptor (owner, group and DACL)
	// in self-relative binary form.
	WinSD      []byte `json:"win_sd,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`

	// Content lists a file's chunks in order. Files with more than
	// inlineContent chunks use ContentIndex: chunks that each hold a run
	// of raw 32-byte ids.
	Content      []keys.ID `json:"content,omitempty"`
	ContentIndex []keys.ID `json:"content_index,omitempty"`

	// BlockSize is the size of each block of an image.
	BlockSize int64 `json:"block_size,omitempty"`
	// HolesZero says the holes of an image were blocks of zeros, as in a
	// virtual disk, and must read as zeros after a restore. Without it
	// holes were free space whose contents do not matter.
	HolesZero bool `json:"holes_zero,omitempty"`

	// Subtree lists the pages holding a directory's entries.
	Subtree []keys.ID `json:"subtree,omitempty"`

	// Streams are the NTFS alternate data streams of a file or directory.
	Streams []Stream `json:"streams,omitempty"`
}

// Stream is one named NTFS alternate data stream. Its data is stored
// like a file's.
type Stream struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	Content      []keys.ID `json:"content,omitempty"`
	ContentIndex []keys.ID `json:"content_index,omitempty"`
}

// Stats summarizes a backup.
type Stats struct {
	Files         int64 `json:"files"`
	Dirs          int64 `json:"dirs"`
	Bytes         int64 `json:"bytes"`
	FilesChanged  int64 `json:"files_changed"`
	BytesRead     int64 `json:"bytes_read"`
	BytesUploaded int64 `json:"bytes_uploaded"`
	ChunksNew     int64 `json:"chunks_new"`
	Warnings      int64 `json:"warnings"`
}

// Snapshot is one completed backup.
type Snapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	AgentID  string    `json:"agent_id"`
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	PolicyID string    `json:"policy_id,omitempty"`
	JobID    string    `json:"job_id,omitempty"`
	// Sources are the backed-up roots; each Name is an absolute path on
	// the source machine.
	Sources []Entry `json:"sources"`
	Stats   Stats   `json:"stats"`
}

// Putter stores a blob and returns its id.
type Putter func(data []byte) (keys.ID, error)

// Getter fetches a blob by id.
type Getter func(id keys.ID) ([]byte, error)

// PageWriter accumulates directory entries and writes them out in pages.
type PageWriter struct {
	put   Putter
	buf   bytes.Buffer
	pages []keys.ID
}

func NewPageWriter(put Putter) *PageWriter { return &PageWriter{put: put} }

// Add appends an entry, flushing a page when it is full.
func (w *PageWriter) Add(e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if w.buf.Len() == 0 {
		w.buf.WriteByte('[')
	} else {
		w.buf.WriteByte(',')
	}
	w.buf.Write(b)
	if w.buf.Len() >= pageSize {
		return w.flush()
	}
	return nil
}

func (w *PageWriter) flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	w.buf.WriteByte(']')
	id, err := w.put(w.buf.Bytes())
	if err != nil {
		return err
	}
	w.pages = append(w.pages, id)
	w.buf.Reset()
	return nil
}

// Finish writes any remaining entries and returns the page ids.
func (w *PageWriter) Finish() ([]keys.ID, error) {
	if err := w.flush(); err != nil {
		return nil, err
	}
	return w.pages, nil
}

// ReadPage decodes one page of entries.
func ReadPage(get Getter, id keys.ID) ([]Entry, error) {
	b, err := get(id)
	if err != nil {
		return nil, err
	}
	var es []Entry
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, fmt.Errorf("directory page %s: %w", id.Short(), err)
	}
	return es, nil
}

// PackIDs prepares a chunk list for storage: short lists stay inline,
// long ones move into index chunks.
func PackIDs(ids []keys.ID, put Putter) (content, index []keys.ID, err error) {
	if len(ids) <= inlineContent {
		return ids, nil, nil
	}
	for len(ids) > 0 {
		n := min(len(ids), idsPerIndex)
		raw := make([]byte, 0, n*32)
		for _, id := range ids[:n] {
			raw = append(raw, id[:]...)
		}
		id, err := put(raw)
		if err != nil {
			return nil, nil, err
		}
		index = append(index, id)
		ids = ids[n:]
	}
	return nil, index, nil
}

// EachID calls fn for every data chunk of a packed chunk list, in order.
func EachID(get Getter, content, index []keys.ID, fn func(keys.ID) error) error {
	for _, id := range content {
		if err := fn(id); err != nil {
			return err
		}
	}
	for _, ix := range index {
		raw, err := get(ix)
		if err != nil {
			return err
		}
		if len(raw)%32 != 0 {
			return fmt.Errorf("content index %s is malformed", ix.Short())
		}
		for ; len(raw) > 0; raw = raw[32:] {
			if err := fn(keys.ID(raw[:32])); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetContent records a file's chunk list on e.
func SetContent(e *Entry, ids []keys.ID, put Putter) (err error) {
	e.Content, e.ContentIndex, err = PackIDs(ids, put)
	return err
}

// EachContent calls fn for every data chunk of a file, in order.
func EachContent(get Getter, e *Entry, fn func(keys.ID) error) error {
	return EachID(get, e.Content, e.ContentIndex, fn)
}

// Mark reports every chunk reachable from the entries: directory pages,
// content indexes and data chunks. Pages already in seen are skipped, so
// subtrees shared between snapshots are walked once.
func Mark(get Getter, entries []Entry, seen map[keys.ID]struct{}, mark func(keys.ID) error) error {
	for i := range entries {
		e := &entries[i]
		for _, st := range e.Streams {
			for _, ix := range st.ContentIndex {
				if err := mark(ix); err != nil {
					return err
				}
			}
			if err := EachID(get, st.Content, st.ContentIndex, mark); err != nil {
				return err
			}
		}
		switch e.Type {
		case TypeFile, TypeImage:
			for _, ix := range e.ContentIndex {
				if err := mark(ix); err != nil {
					return err
				}
			}
			err := EachContent(get, e, func(id keys.ID) error {
				if id == (keys.ID{}) {
					return nil // an image hole
				}
				return mark(id)
			})
			if err != nil {
				return err
			}
		case TypeDir:
			for _, p := range e.Subtree {
				if _, ok := seen[p]; ok {
					continue
				}
				seen[p] = struct{}{}
				if err := mark(p); err != nil {
					return err
				}
				sub, err := ReadPage(get, p)
				if err != nil {
					return err
				}
				if err := Mark(get, sub, seen, mark); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
