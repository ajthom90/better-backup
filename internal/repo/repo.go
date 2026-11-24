// Package repo implements the backup repository: a content-addressed,
// deduplicated, compressed and encrypted chunk store.
//
// On disk a repository is:
//
//	config.json          repository id and the passphrase-wrapped master key
//	packs/ab/<id>.pack   pack files, each holding many encrypted chunks
//	snapshots/<id>.snap  encrypted snapshot records
//	index.db             SQLite index of chunk -> pack location (a cache:
//	                     it is rebuilt from the pack trailers if missing)
package repo

import (
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

const (
	formatVersion = 1
	packMagic     = "BBPACK1\n"
	packEndMagic  = "BBPKEND1"
	entrySize     = 32 + 8 + 4 + 4

	// DefaultPackSize is the size at which a pack file is closed.
	DefaultPackSize = 32 << 20
	// MaxChunkSize bounds a single chunk accepted by Put.
	MaxChunkSize = 16 << 20
)

// ErrNotFound is returned when a chunk is not in the repository.
var ErrNotFound = errors.New("chunk not found")

type config struct {
	Version int           `json:"version"`
	ID      string        `json:"id"`
	Created time.Time     `json:"created"`
	Key     *keys.Wrapped `json:"key"`
}

type packID [16]byte

func (p packID) String() string { return hex.EncodeToString(p[:]) }

type entry struct {
	id     keys.ID
	off    int64
	length uint32
	rawLen uint32
}

type openPack struct {
	id      packID
	f       *os.File
	path    string
	size    int64
	entries []entry
}

// Repo is an open repository. It is safe for concurrent use, except that
// GC must not run concurrently with Put or Get.
type Repo struct {
	dir  string
	id   string
	keys keys.Keys
	db   *sql.DB
	enc  *zstd.Encoder
	dec  *zstd.Decoder

	// PackSize may be lowered in tests.
	PackSize int64
	readOnly bool

	mu      sync.Mutex
	cur     *openPack
	pending map[keys.ID]entry
}

// Exists reports whether dir holds a repository.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "config.json"))
	return err == nil
}

// Init creates a new repository in dir, protected by passphrase.
func Init(dir string, passphrase []byte) error {
	if Exists(dir) {
		return fmt.Errorf("a repository already exists in %s", dir)
	}
	for _, d := range []string{"packs", "snapshots", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	cfg := config{Version: formatVersion, ID: keys.RandomHex(16), Created: time.Now().UTC(), Key: keys.Wrap(passphrase, keys.Random(32))}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return writeFileAtomic(filepath.Join(dir, "config.json"), b)
}

// Open opens the repository in dir for reading and writing. Only one
// process may have a repository open this way.
func Open(dir string, passphrase []byte) (*Repo, error) { return open(dir, passphrase, false) }

// OpenReadOnly opens the repository for reading. It is safe while a
// server has the repository open, which is what makes an emergency
// restore possible without stopping anything.
func OpenReadOnly(dir string, passphrase []byte) (*Repo, error) { return open(dir, passphrase, true) }

func open(dir string, passphrase []byte, readOnly bool) (*Repo, error) {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("repository config: %w", err)
	}
	if cfg.Version != formatVersion {
		return nil, fmt.Errorf("repository format version %d is not supported by this build", cfg.Version)
	}
	master, err := keys.Unwrap(passphrase, cfg.Key)
	if err != nil {
		return nil, err
	}
	r := &Repo{dir: dir, id: cfg.ID, keys: keys.Derive(master), PackSize: DefaultPackSize, pending: map[keys.ID]entry{}, readOnly: readOnly}
	r.enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	r.dec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxChunkSize))

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "index.db")) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(30000)"
	if r.db, err = sql.Open("sqlite", dsn); err != nil {
		return nil, err
	}
	if _, err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS packs (id BLOB PRIMARY KEY, size INTEGER NOT NULL) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS chunks (id BLOB PRIMARY KEY, pack BLOB NOT NULL, off INTEGER NOT NULL,
			len INTEGER NOT NULL, rawlen INTEGER NOT NULL) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS chunks_pack ON chunks(pack);`); err != nil {
		r.db.Close()
		return nil, fmt.Errorf("repository index: %w", err)
	}
	if err := r.reconcile(); err != nil {
		r.db.Close()
		return nil, err
	}
	return r, nil
}

// ID returns the repository's unique id.
func (r *Repo) ID() string { return r.id }

// Keys returns the working keys.
func (r *Repo) Keys() keys.Keys { return r.keys }

// Close flushes pending data and closes the repository.
func (r *Repo) Close() error {
	err := r.Flush()
	if cerr := r.db.Close(); err == nil {
		err = cerr
	}
	return err
}

func (r *Repo) packPath(id packID) string {
	s := id.String()
	return filepath.Join(r.dir, "packs", s[:2], s+".pack")
}

// reconcile indexes pack files the index does not know about (after a
// crash, or when index.db was deleted) and clears stale temp files.
func (r *Repo) reconcile() error {
	if !r.readOnly {
		tmps, _ := filepath.Glob(filepath.Join(r.dir, "tmp", "*"))
		for _, t := range tmps {
			os.Remove(t)
		}
	}
	known := map[packID]bool{}
	rows, err := r.db.Query(`SELECT id FROM packs`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		known[packID(b)] = true
	}
	rows.Close()
	files, _ := filepath.Glob(filepath.Join(r.dir, "packs", "*", "*.pack"))
	for _, f := range files {
		raw, err := hex.DecodeString(strings.TrimSuffix(filepath.Base(f), ".pack"))
		if err != nil || len(raw) != 16 {
			continue
		}
		id := packID(raw)
		if known[id] {
			continue
		}
		entries, size, err := r.readTrailer(f, id)
		if err != nil {
			return fmt.Errorf("pack %s is not indexed and cannot be read: %w", id, err)
		}
		if err := r.indexPack(id, size, entries); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) indexPack(id packID, size int64, entries []entry) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO packs (id, size) VALUES (?, ?)`, id[:], size); err != nil {
		return err
	}
	st, err := tx.Prepare(`INSERT OR REPLACE INTO chunks (id, pack, off, len, rawlen) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, e := range entries {
		if _, err := st.Exec(e.id[:], id[:], e.off, e.length, e.rawLen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Repo) lookup(id keys.ID) (packID, entry, bool, error) {
	var p []byte
	e := entry{id: id}
	err := r.db.QueryRow(`SELECT pack, off, len, rawlen FROM chunks WHERE id = ?`, id[:]).Scan(&p, &e.off, &e.length, &e.rawLen)
	if errors.Is(err, sql.ErrNoRows) {
		return packID{}, e, false, nil
	}
	if err != nil {
		return packID{}, e, false, err
	}
	return packID(p), e, true, nil
}

// Has reports whether the chunk is stored.
func (r *Repo) Has(id keys.ID) (bool, error) {
	r.mu.Lock()
	_, ok := r.pending[id]
	r.mu.Unlock()
	if ok {
		return true, nil
	}
	_, _, ok, err := r.lookup(id)
	return ok, err
}

// Missing returns the subset of ids that are not stored.
func (r *Repo) Missing(ids []keys.ID) ([]keys.ID, error) {
	missing := []keys.ID{}
	for _, id := range ids {
		ok, err := r.Has(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// PutData stores data and returns its id.
func (r *Repo) PutData(data []byte) (keys.ID, error) {
	id := keys.ChunkID(&r.keys.ID, data)
	return id, r.Put(id, data)
}

// Put stores a chunk. The id is verified against the data, so a client
// cannot poison the shared store with mislabeled content.
func (r *Repo) Put(id keys.ID, data []byte) error {
	if r.readOnly {
		return errors.New("repository is open read-only")
	}
	if len(data) > MaxChunkSize {
		return fmt.Errorf("chunk of %d bytes exceeds the %d byte limit", len(data), MaxChunkSize)
	}
	if keys.ChunkID(&r.keys.ID, data) != id {
		return fmt.Errorf("chunk %s does not match its content", id.Short())
	}
	if ok, err := r.Has(id); err != nil || ok {
		return err
	}
	plain := make([]byte, 1, len(data)+1)
	if comp := r.enc.EncodeAll(data, nil); len(comp) < len(data) {
		plain[0] = 1
		plain = append(plain, comp...)
	} else {
		plain = append(plain, data...)
	}
	sealed := keys.Seal(&r.keys.Enc, id[:], plain)

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[id]; ok {
		return nil
	}
	return r.appendLocked(id, sealed, uint32(len(data)))
}

func (r *Repo) appendLocked(id keys.ID, sealed []byte, rawLen uint32) error {
	if r.cur == nil {
		p := &openPack{id: packID(keys.Random(16))}
		p.path = filepath.Join(r.dir, "tmp", p.id.String()+".pack")
		f, err := os.OpenFile(p.path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write([]byte(packMagic)); err != nil {
			f.Close()
			os.Remove(p.path)
			return err
		}
		p.f, p.size = f, int64(len(packMagic))
		r.cur = p
	}
	p := r.cur
	if _, err := p.f.WriteAt(sealed, p.size); err != nil {
		return fmt.Errorf("write pack: %w", err)
	}
	e := entry{id: id, off: p.size, length: uint32(len(sealed)), rawLen: rawLen}
	p.size += int64(len(sealed))
	p.entries = append(p.entries, e)
	r.pending[id] = e
	if p.size >= r.PackSize {
		return r.finishLocked()
	}
	return nil
}

// finishLocked seals the open pack: trailer, fsync, rename into place,
// then index. A crash before the index commit is repaired by reconcile.
func (r *Repo) finishLocked() error {
	p := r.cur
	if p == nil {
		return nil
	}
	raw := make([]byte, 0, len(p.entries)*entrySize)
	for _, e := range p.entries {
		raw = append(raw, e.id[:]...)
		raw = binary.BigEndian.AppendUint64(raw, uint64(e.off))
		raw = binary.BigEndian.AppendUint32(raw, e.length)
		raw = binary.BigEndian.AppendUint32(raw, e.rawLen)
	}
	trailer := keys.Seal(&r.keys.Enc, append([]byte("pack-index:"), p.id[:]...), raw)
	trailer = binary.BigEndian.AppendUint32(trailer, uint32(len(trailer)))
	trailer = append(trailer, packEndMagic...)
	if _, err := p.f.WriteAt(trailer, p.size); err != nil {
		return fmt.Errorf("write pack trailer: %w", err)
	}
	size := p.size + int64(len(trailer))
	if err := p.f.Sync(); err != nil {
		return err
	}
	if err := p.f.Close(); err != nil {
		return err
	}
	final := r.packPath(p.id)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := os.Rename(p.path, final); err != nil {
		return err
	}
	syncDir(filepath.Dir(final))
	if err := r.indexPack(p.id, size, p.entries); err != nil {
		return err
	}
	for _, e := range p.entries {
		delete(r.pending, e.id)
	}
	r.cur = nil
	return nil
}

// Flush makes every chunk stored so far durable.
func (r *Repo) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finishLocked()
}

func (r *Repo) readSealed(id keys.ID) ([]byte, uint32, error) {
	r.mu.Lock()
	if e, ok := r.pending[id]; ok {
		buf := make([]byte, e.length)
		_, err := r.cur.f.ReadAt(buf, e.off)
		r.mu.Unlock()
		return buf, e.rawLen, err
	}
	r.mu.Unlock()
	pid, e, ok, err := r.lookup(id)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, id.Short())
	}
	f, err := os.Open(r.packPath(pid))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	buf := make([]byte, e.length)
	if _, err := f.ReadAt(buf, e.off); err != nil {
		return nil, 0, fmt.Errorf("read pack %s: %w", pid, err)
	}
	return buf, e.rawLen, nil
}

func (r *Repo) unseal(id keys.ID, sealed []byte) ([]byte, error) {
	plain, err := keys.Open(&r.keys.Enc, id[:], sealed)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", id.Short(), err)
	}
	if len(plain) == 0 {
		return nil, fmt.Errorf("chunk %s is empty", id.Short())
	}
	switch plain[0] {
	case 0:
		return plain[1:], nil
	case 1:
		out, err := r.dec.DecodeAll(plain[1:], nil)
		if err != nil {
			return nil, fmt.Errorf("chunk %s: decompress: %w", id.Short(), err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("chunk %s has unknown encoding %d", id.Short(), plain[0])
}

// Get returns a chunk's plaintext.
func (r *Repo) Get(id keys.ID) ([]byte, error) {
	sealed, _, err := r.readSealed(id)
	if err != nil {
		return nil, err
	}
	return r.unseal(id, sealed)
}

func (r *Repo) readTrailer(path string, id packID) ([]entry, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if size < int64(len(packMagic))+12 {
		return nil, 0, errors.New("pack file is truncated")
	}
	tail := make([]byte, 12)
	if _, err := f.ReadAt(tail, size-12); err != nil {
		return nil, 0, err
	}
	if string(tail[4:]) != packEndMagic {
		return nil, 0, errors.New("pack file has no trailer")
	}
	n := int64(binary.BigEndian.Uint32(tail))
	if n > size-12-int64(len(packMagic)) {
		return nil, 0, errors.New("pack trailer length is invalid")
	}
	sealed := make([]byte, n)
	if _, err := f.ReadAt(sealed, size-12-n); err != nil {
		return nil, 0, err
	}
	raw, err := keys.Open(&r.keys.Enc, append([]byte("pack-index:"), id[:]...), sealed)
	if err != nil {
		return nil, 0, err
	}
	if len(raw)%entrySize != 0 {
		return nil, 0, errors.New("pack trailer is malformed")
	}
	entries := make([]entry, 0, len(raw)/entrySize)
	for ; len(raw) > 0; raw = raw[entrySize:] {
		entries = append(entries, entry{
			id:     keys.ID(raw[:32]),
			off:    int64(binary.BigEndian.Uint64(raw[32:])),
			length: binary.BigEndian.Uint32(raw[40:]),
			rawLen: binary.BigEndian.Uint32(raw[44:]),
		})
	}
	return entries, size, nil
}

// Stats describes repository usage.
type Stats struct {
	Packs       int64 `json:"packs"`
	StoredBytes int64 `json:"stored_bytes"`
	Chunks      int64 `json:"chunks"`
	RawBytes    int64 `json:"raw_bytes"`
}

// Stats returns usage totals.
func (r *Repo) Stats() (Stats, error) {
	var s Stats
	if err := r.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM packs`).Scan(&s.Packs, &s.StoredBytes); err != nil {
		return s, err
	}
	err := r.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(rawlen), 0) FROM chunks`).Scan(&s.Chunks, &s.RawBytes)
	return s, err
}

// SaveSnapshot writes a snapshot record. All chunks are flushed first so
// a snapshot never refers to data that is not yet durable.
func (r *Repo) SaveSnapshot(s *tree.Snapshot) error {
	if err := r.Flush(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	sealed := keys.Seal(&r.keys.Enc, []byte("snapshot:"+s.ID), b)
	return writeFileAtomic(filepath.Join(r.dir, "snapshots", s.ID+".snap"), sealed)
}

// LoadSnapshots reads every snapshot record.
func (r *Repo) LoadSnapshots() ([]*tree.Snapshot, error) {
	files, err := filepath.Glob(filepath.Join(r.dir, "snapshots", "*.snap"))
	if err != nil {
		return nil, err
	}
	var out []*tree.Snapshot
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".snap")
		sealed, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		b, err := keys.Open(&r.keys.Enc, []byte("snapshot:"+id), sealed)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", id, err)
		}
		s := new(tree.Snapshot)
		if err := json.Unmarshal(b, s); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", id, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// DeleteSnapshot removes a snapshot record. Its data is reclaimed by GC.
func (r *Repo) DeleteSnapshot(id string) error {
	if strings.ContainsAny(id, `/\.`) {
		return fmt.Errorf("invalid snapshot id %q", id)
	}
	err := os.Remove(filepath.Join(r.dir, "snapshots", id+".snap"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp-" + keys.RandomHex(4)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes a rename durable. Directories cannot be synced on
// Windows, where the error is ignored.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// KeysFromConfig derives a repository's working keys from the contents
// of its config.json. It fails when the passphrase is wrong, which makes
// it the cheap way to check a key before anything else is read.
func KeysFromConfig(configJSON, passphrase []byte) (keys.Keys, error) {
	var cfg config
	if err := json.Unmarshal(configJSON, &cfg); err != nil || cfg.Key == nil {
		return keys.Keys{}, errors.New("config.json is not a repository's configuration")
	}
	master, err := keys.Unwrap(passphrase, cfg.Key)
	if err != nil {
		return keys.Keys{}, err
	}
	return keys.Derive(master), nil
}
