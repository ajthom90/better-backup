// Package searchindex keeps an index of the names of everything that is
// backed up, so that a file is found by name without reading the
// snapshots.
//
// The index is a SQLite file next to the server's database. It holds
// nothing that is not in the repository and is built again from the
// snapshots whenever it is missing.
//
// Its size follows the number of different paths, not the number of
// snapshots: a path is one row however many snapshots hold it. The row
// says from which snapshot to which the path was there. Adding a
// snapshot reads only the directories that changed since the snapshot
// before it, because a directory that did not change has the same
// pages, and forgetting a snapshot writes nothing but a mark.
package searchindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// How the index is laid out.
//
// series: the snapshots of one policy of one agent, which follow one
// another. A snapshot is compared with the one before it in its series.
//
// snaps: every snapshot the index has seen, numbered in the order it
// was added (seq). A forgotten snapshot stays, marked gone, because
// rows still name its number as the place a path began or ended.
//
// nodes: one row per path: a file, a folder or a link. first and last
// are the numbers of the first and last snapshot of the series that
// hold it; last is NULL while the newest indexed snapshot still holds
// it. A top-level source has minus its series as its parent. sub is a
// short hash of the pages of a folder as last indexed, and NULL while
// the folder is half read.
//
// past: the earlier times a path was there, for a path that went away
// and came back.
//
// names: a trigram index over the names, which finds any part of a
// name of three letters or more.
const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS series (id INTEGER PRIMARY KEY, agent TEXT NOT NULL, policy TEXT NOT NULL, UNIQUE (agent, policy));
CREATE TABLE IF NOT EXISTS snaps (
	seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, series INTEGER NOT NULL,
	time INTEGER NOT NULL, done INTEGER NOT NULL DEFAULT 0, gone INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS snaps_series ON snaps (series, seq);
CREATE TABLE IF NOT EXISTS nodes (
	id INTEGER PRIMARY KEY, parent INTEGER NOT NULL, name TEXT NOT NULL, series INTEGER NOT NULL,
	kind INTEGER NOT NULL, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
	first INTEGER NOT NULL, last INTEGER, sub INTEGER);
CREATE UNIQUE INDEX IF NOT EXISTS nodes_parent ON nodes (parent, name);
CREATE INDEX IF NOT EXISTS nodes_closed ON nodes (first) WHERE last IS NOT NULL;
CREATE TABLE IF NOT EXISTS past (node INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL, PRIMARY KEY (node, first)) WITHOUT ROWID;
CREATE VIRTUAL TABLE IF NOT EXISTS names USING fts5 (name, content='nodes', content_rowid='id', tokenize='trigram', detail=none, columnsize=0);
`

const version = "1"

// batch is how many rows are written before they are committed. A
// snapshot that is half indexed is picked up where it stopped.
const batch = 20000

var kinds = []string{tree.TypeFile, tree.TypeDir, tree.TypeSymlink, tree.TypeImage, tree.TypeVM, tree.TypeMachine}

func kindOf(t string) int {
	for i, k := range kinds {
		if k == t {
			return i
		}
	}
	return 0
}

// Index is an open search index.
type Index struct {
	path string
	// w is the one connection that writes; r reads while it does.
	w, r *sql.DB
	wmu  sync.Mutex // one change at a time

	mu    sync.Mutex
	names int64
	live  map[int64]*line // by series; nil when it must be read again
	st    stmts
}

// line is the snapshots of one series that still exist, oldest first.
type line struct {
	agent string
	seqs  []int64
	ids   []string
}

type stmts struct {
	kids, insert, fts, update, reopen, past, setSub, closeKids, dirKids, closeOne *sql.Stmt
}

func open(path string, readOnly bool) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(30000)"
	// SQLite keeps 2 MB of the file in memory unless told otherwise,
	// which is little for an index of millions of names.
	if readOnly {
		dsn += "&_pragma=query_only(1)&_pragma=cache_size(-16384)"
	} else {
		dsn += "&_pragma=cache_size(-65536)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if readOnly {
		db.SetMaxOpenConns(4)
	} else {
		db.SetMaxOpenConns(1)
	}
	return db, nil
}

// Open opens the index at path, creating it when it is not there. repo
// names the repository the index is of: an index of another repository,
// or one written by a different version, is emptied.
func Open(path, repo string) (*Index, error) {
	ix := &Index{path: path}
	var err error
	if ix.w, err = open(path, false); err != nil {
		return nil, err
	}
	if _, err = ix.w.Exec(schema); err != nil {
		// A damaged index is no loss: start again.
		ix.w.Close()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suffix)
		}
		if ix.w, err = open(path, false); err != nil {
			return nil, err
		}
		if _, err = ix.w.Exec(schema); err != nil {
			ix.w.Close()
			return nil, err
		}
	}
	if ix.r, err = open(path, true); err != nil {
		ix.w.Close()
		return nil, err
	}
	want := version + " " + repo
	if got := ix.meta("of"); got != want {
		if got != "" {
			if err := ix.reset(); err != nil {
				ix.Close()
				return nil, err
			}
		}
		if err := ix.setMeta("of", want); err != nil {
			ix.Close()
			return nil, err
		}
	}
	fmt.Sscan(ix.meta("names"), &ix.names)
	if err := ix.prepare(); err != nil {
		ix.Close()
		return nil, err
	}
	return ix, nil
}

func (ix *Index) prepare() error {
	var err error
	prep := func(q string) *sql.Stmt {
		if err != nil {
			return nil
		}
		var st *sql.Stmt
		st, err = ix.w.Prepare(q)
		return st
	}
	ix.st = stmts{
		kids:      prep(`SELECT id, name, kind, size, mtime, first, last, sub FROM nodes WHERE parent = ?`),
		insert:    prep(`INSERT INTO nodes (parent, name, series, kind, size, mtime, first) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		fts:       prep(`INSERT INTO names (rowid, name) VALUES (?, ?)`),
		update:    prep(`UPDATE nodes SET kind = ?, size = ?, mtime = ? WHERE id = ?`),
		reopen:    prep(`UPDATE nodes SET first = ?, last = NULL, sub = NULL WHERE id = ?`),
		past:      prep(`INSERT OR REPLACE INTO past (node, first, last) VALUES (?, ?, ?)`),
		setSub:    prep(`UPDATE nodes SET sub = ? WHERE id = ?`),
		dirKids:   prep(`SELECT id FROM nodes WHERE parent = ? AND last IS NULL AND kind = 1`),
		closeKids: prep(`UPDATE nodes SET last = ? WHERE parent = ? AND last IS NULL`),
		closeOne:  prep(`UPDATE nodes SET last = ? WHERE id = ?`),
	}
	return err
}

// Close closes the index.
func (ix *Index) Close() error {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	if ix.r != nil {
		ix.r.Close()
	}
	return ix.w.Close()
}

// Meta returns a note kept with the index, and SetMeta keeps one. The
// notes go when the index is emptied.
func (ix *Index) Meta(key string) string {
	var v string
	ix.r.QueryRow(`SELECT value FROM meta WHERE key = ?`, "x-"+key).Scan(&v)
	return v
}

func (ix *Index) SetMeta(key, value string) error {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	return ix.setMeta("x-"+key, value)
}

func (ix *Index) meta(key string) string {
	var v string
	ix.w.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

func (ix *Index) setMeta(key, value string) error {
	_, err := ix.w.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (ix *Index) changed() {
	ix.mu.Lock()
	ix.live = nil
	ix.mu.Unlock()
}

// reset empties the index. The caller holds wmu, or nobody else has
// the index yet.
func (ix *Index) reset() error {
	for _, q := range []string{`DROP TABLE IF EXISTS names`, `DROP TABLE IF EXISTS nodes`, `DROP TABLE IF EXISTS past`, `DROP TABLE IF EXISTS snaps`,
		`DROP TABLE IF EXISTS series`, `DELETE FROM meta WHERE key != 'of'`, schema, `VACUUM`} {
		if _, err := ix.w.Exec(q); err != nil {
			return err
		}
	}
	ix.mu.Lock()
	ix.names, ix.live = 0, nil
	ix.mu.Unlock()
	return nil
}

// Reset empties the index, to have it built again from the snapshots.
func (ix *Index) Reset() error {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	for _, st := range []*sql.Stmt{ix.st.kids, ix.st.insert, ix.st.fts, ix.st.update, ix.st.reopen, ix.st.past, ix.st.setSub, ix.st.dirKids, ix.st.closeKids, ix.st.closeOne} {
		if st != nil {
			st.Close()
		}
	}
	if err := ix.reset(); err != nil {
		return err
	}
	return ix.prepare()
}

// Status says how much the index holds.
type Status struct {
	// Snapshots is how many snapshots are in the index and still exist.
	Snapshots int
	// Names is how many different paths the index holds.
	Names int64
	// Bytes is the size of the index on disk.
	Bytes int64
}

func (ix *Index) Status() Status {
	var st Status
	ix.r.QueryRow(`SELECT COUNT(*) FROM snaps WHERE done = 1 AND gone = 0`).Scan(&st.Snapshots)
	ix.mu.Lock()
	st.Names = ix.names
	ix.mu.Unlock()
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(ix.path + suffix); err == nil {
			st.Bytes += fi.Size()
		}
	}
	return st
}

// tx runs fn in one transaction on the writing connection.
func (ix *Index) tx(fn func() error) error {
	if _, err := ix.w.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if err := fn(); err != nil {
		ix.w.Exec(`ROLLBACK`)
		return err
	}
	_, err := ix.w.Exec(`COMMIT`)
	return err
}

func seriesKey(sn *tree.Snapshot) string { return sn.AgentID + "\x00" + sn.PolicyID }

// Reconcile compares the index with the snapshots that exist. It marks
// the ones that are gone, drops what only they held, and returns the
// ones that are not in the index yet, in the order to Add them.
func (ix *Index) Reconcile(snaps []*tree.Snapshot) ([]*tree.Snapshot, error) {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	defer ix.changed()
	exists := make(map[string]*tree.Snapshot, len(snaps))
	for _, sn := range snaps {
		exists[sn.ID] = sn
	}
	type row struct {
		seq, series, time int64
		id                string
		done, gone        bool
	}
	rows, err := ix.w.Query(`SELECT seq, id, series, time, done, gone FROM snaps ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.id, &r.series, &r.time, &r.done, &r.gone); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	seriesOf := map[string]int64{}
	rows, err = ix.w.Query(`SELECT id, agent, policy FROM series`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var agent, policy string
		rows.Scan(&id, &agent, &policy)
		seriesOf[agent+"\x00"+policy] = id
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	indexed := map[string]bool{}
	alive := map[int64]int{}    // snapshots of a series that exist
	newest := map[int64]int64{} // time of the newest indexed snapshot of a series
	var lost []int64            // snapshots that are newly gone
	// A series is indexed again from its start if a snapshot older than
	// one already indexed is to be added, which cannot be done after
	// it, or if a forgotten snapshot is back, as after the repository
	// was put back from a copy.
	again := map[int64]bool{}
	for _, r := range all {
		switch {
		case r.gone:
			if exists[r.id] != nil {
				again[r.series] = true
			}
		case exists[r.id] == nil:
			lost = append(lost, r.seq)
		default:
			alive[r.series]++
			if r.done {
				indexed[r.id] = true
			}
		}
		if r.done && r.time > newest[r.series] {
			newest[r.series] = r.time
		}
	}
	var pending []*tree.Snapshot
	for _, sn := range snaps {
		if !indexed[sn.ID] {
			pending = append(pending, sn)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].Time.Equal(pending[j].Time) {
			return pending[i].Time.Before(pending[j].Time)
		}
		return pending[i].ID < pending[j].ID
	})
	waiting := map[int64]bool{}
	for _, sn := range pending {
		if id, ok := seriesOf[seriesKey(sn)]; ok {
			waiting[id] = true
			if sn.Time.UnixMilli() < newest[id] {
				again[id] = true
			}
		}
	}
	if len(again) > 0 {
		pending = pending[:0]
		for _, sn := range snaps {
			if !indexed[sn.ID] || again[seriesOf[seriesKey(sn)]] {
				pending = append(pending, sn)
			}
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].Time.Before(pending[j].Time) })
	}
	// A series none of whose snapshots exist is dropped whole, unless a
	// new snapshot of it waits: that one is added faster to what is there.
	var drop []int64
	for _, id := range seriesOf {
		if (alive[id] == 0 && !waiting[id]) || again[id] {
			drop = append(drop, id)
		}
	}
	if len(lost) == 0 && len(drop) == 0 {
		return pending, nil
	}
	err = ix.tx(func() error {
		for _, seq := range lost {
			if _, err := ix.w.Exec(`UPDATE snaps SET gone = 1 WHERE seq = ?`, seq); err != nil {
				return err
			}
		}
		for _, id := range drop {
			if err := ix.dropSeries(id, !again[id]); err != nil {
				return err
			}
		}
		if err := ix.sweep(); err != nil {
			return err
		}
		return ix.saveNames()
	})
	if err != nil {
		// The count in memory may be ahead of the file.
		ix.mu.Lock()
		fmt.Sscan(ix.meta("names"), &ix.names)
		ix.mu.Unlock()
		return nil, err
	}
	return pending, nil
}

func (ix *Index) saveNames() error {
	ix.mu.Lock()
	n := ix.names
	ix.mu.Unlock()
	return ix.setMeta("names", fmt.Sprint(n))
}

func (ix *Index) addNames(n int64) {
	ix.mu.Lock()
	ix.names += n
	ix.mu.Unlock()
}

// dropSeries removes everything of a series, inside a transaction.
func (ix *Index) dropSeries(id int64, forget bool) error {
	var n int64
	if err := ix.w.QueryRow(`SELECT COUNT(*) FROM nodes WHERE series = ?`, id).Scan(&n); err != nil {
		return err
	}
	qs := []string{
		`INSERT INTO names (names, rowid, name) SELECT 'delete', id, name FROM nodes WHERE series = ?`,
		`DELETE FROM past WHERE node IN (SELECT id FROM nodes WHERE series = ?)`,
		`DELETE FROM nodes WHERE series = ?`,
		`DELETE FROM snaps WHERE series = ?`,
	}
	if forget {
		qs = append(qs, `DELETE FROM series WHERE id = ?`)
	}
	for _, q := range qs {
		if _, err := ix.w.Exec(q, id); err != nil {
			return err
		}
	}
	ix.addNames(-n)
	return nil
}

// sweep removes the paths that only forgotten snapshots held, inside a
// transaction. A path is looked at only if it has gone away and began
// in a forgotten snapshot, so the work follows what was forgotten, not
// the size of the index.
func (ix *Index) sweep() error {
	// next: for each forgotten snapshot, the first later snapshot of
	// its series that still exists.
	if _, err := ix.w.Exec(`CREATE TEMP TABLE IF NOT EXISTS lost (seq INTEGER PRIMARY KEY, next INTEGER NOT NULL)`); err != nil {
		return err
	}
	if _, err := ix.w.Exec(`DELETE FROM lost`); err != nil {
		return err
	}
	if _, err := ix.w.Exec(`INSERT INTO lost SELECT g.seq, COALESCE((SELECT MIN(a.seq) FROM snaps a WHERE a.series = g.series AND a.gone = 0 AND a.seq > g.seq), ?)
		FROM snaps g WHERE g.gone = 1`, int64(math.MaxInt64)); err != nil {
		return err
	}
	const dead = `first IN (SELECT seq FROM lost) AND last < (SELECT next FROM lost WHERE seq = first)`
	if _, err := ix.w.Exec(`DELETE FROM past WHERE ` + dead); err != nil {
		return err
	}
	// A path whose last time is dead takes its time before that, if it
	// has one, and is removed if not.
	rows, err := ix.w.Query(`SELECT id, name FROM nodes WHERE last IS NOT NULL AND ` + dead)
	if err != nil {
		return err
	}
	type gone struct {
		id   int64
		name string
	}
	var all []gone
	for rows.Next() {
		var g gone
		rows.Scan(&g.id, &g.name)
		all = append(all, g)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, g := range all {
		var first, last int64
		err := ix.w.QueryRow(`SELECT first, last FROM past WHERE node = ? ORDER BY first DESC LIMIT 1`, g.id).Scan(&first, &last)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := ix.w.Exec(`INSERT INTO names (names, rowid, name) VALUES ('delete', ?, ?)`, g.id, g.name); err != nil {
				return err
			}
			if _, err := ix.w.Exec(`DELETE FROM nodes WHERE id = ?`, g.id); err != nil {
				return err
			}
			ix.addNames(-1)
		case err != nil:
			return err
		default:
			if _, err := ix.w.Exec(`UPDATE nodes SET first = ?, last = ? WHERE id = ?`, first, last, g.id); err != nil {
				return err
			}
			if _, err := ix.w.Exec(`DELETE FROM past WHERE node = ? AND first = ?`, g.id, first); err != nil {
				return err
			}
		}
	}
	return nil
}

// entry is what the index reads of a directory entry.
type entry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime int64     `json:"mtime"`
	Subtree []keys.ID `json:"subtree"`
}

// subHash is a short hash of the pages of a folder. Two versions of a
// folder with the same hash hold the same things all the way down.
func subHash(pages []keys.ID) int64 {
	h := sha256.New()
	for _, p := range pages {
		h.Write(p[:])
	}
	return int64(binary.LittleEndian.Uint64(h.Sum(nil)))
}

type adder struct {
	ix     *Index
	ctx    context.Context
	get    tree.Getter
	series int64
	seq    int64 // the snapshot being added
	prev   int64 // the one before it in its series
	writes int
	added  int64
}

type old struct {
	id, size, mtime, first int64
	kind                   int
	last, sub              sql.NullInt64
	seen                   bool
}

// wrote counts a write, and commits when enough have gathered.
func (a *adder) wrote() error {
	a.writes++
	if a.writes < batch {
		return nil
	}
	a.writes = 0
	if err := a.commit(); err != nil {
		return err
	}
	if err := a.ctx.Err(); err != nil {
		return err
	}
	_, err := a.ix.w.Exec(`BEGIN IMMEDIATE`)
	return err
}

func (a *adder) commit() error {
	a.ix.addNames(a.added)
	a.added = 0
	if err := a.ix.saveNames(); err != nil {
		return err
	}
	_, err := a.ix.w.Exec(`COMMIT`)
	return err
}

// Add puts a snapshot into the index. The snapshots of a series are
// added oldest first; Reconcile gives the order. If Add is stopped part
// of the way it goes on from there when called again.
func (ix *Index) Add(ctx context.Context, get tree.Getter, sn *tree.Snapshot) (err error) {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	defer ix.changed()
	a := &adder{ix: ix, ctx: ctx, get: get}
	if _, err := ix.w.Exec(`INSERT OR IGNORE INTO series (agent, policy) VALUES (?, ?)`, sn.AgentID, sn.PolicyID); err != nil {
		return err
	}
	if err := ix.w.QueryRow(`SELECT id FROM series WHERE agent = ? AND policy = ?`, sn.AgentID, sn.PolicyID).Scan(&a.series); err != nil {
		return err
	}
	if _, err := ix.w.Exec(`INSERT OR IGNORE INTO snaps (id, series, time) VALUES (?, ?, ?)`, sn.ID, a.series, sn.Time.UnixMilli()); err != nil {
		return err
	}
	var done bool
	if err := ix.w.QueryRow(`SELECT seq, done FROM snaps WHERE id = ?`, sn.ID).Scan(&a.seq, &done); err != nil {
		return err
	}
	if done {
		return nil
	}
	if err := ix.w.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM snaps WHERE series = ? AND done = 1 AND seq < ?`, a.series, a.seq).Scan(&a.prev); err != nil {
		return err
	}
	if _, err := ix.w.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Keep what was written: the next call goes on from it.
			if cerr := a.commit(); cerr != nil {
				ix.w.Exec(`ROLLBACK`)
				ix.mu.Lock()
				fmt.Sscan(ix.meta("names"), &ix.names)
				ix.mu.Unlock()
			}
		}
	}()
	// The sources are the children of the series itself.
	srcs := make([]entry, len(sn.Sources))
	for i := range sn.Sources {
		s := &sn.Sources[i]
		srcs[i] = entry{Name: s.Name, Type: s.Type, Size: s.Size, ModTime: s.ModTime, Subtree: s.Subtree}
	}
	if err := a.dir(-a.series, func(each func([]entry) error) error { return each(srcs) }); err != nil {
		return err
	}
	if _, err := ix.w.Exec(`UPDATE snaps SET done = 1 WHERE seq = ?`, a.seq); err != nil {
		return err
	}
	return a.commit()
}

// dir brings the children of one folder in the index into line with
// the entries that pages hands over.
func (a *adder) dir(parent int64, pages func(each func([]entry) error) error) error {
	st := &a.ix.st
	rows, err := st.kids.Query(parent)
	if err != nil {
		return err
	}
	olds := map[string]*old{}
	for rows.Next() {
		o := new(old)
		var name string
		if err := rows.Scan(&o.id, &name, &o.kind, &o.size, &o.mtime, &o.first, &o.last, &o.sub); err != nil {
			rows.Close()
			return err
		}
		olds[name] = o
	}
	if err := rows.Close(); err != nil {
		return err
	}
	type todo struct {
		id    int64
		pages []keys.ID
	}
	var below []todo
	err = pages(func(es []entry) error {
		for i := range es {
			e := &es[i]
			kind, mtime := kindOf(e.Type), e.ModTime/1e9
			size := e.Size
			if kind == 1 {
				size = 0
			}
			o := olds[e.Name]
			switch {
			case o == nil:
				res, err := st.insert.Exec(parent, e.Name, a.series, kind, size, mtime, a.seq)
				if err != nil {
					return err
				}
				id, _ := res.LastInsertId()
				if _, err := st.fts.Exec(id, e.Name); err != nil {
					return err
				}
				a.added++
				olds[e.Name] = &old{id: id, seen: true}
				if kind == 1 {
					below = append(below, todo{id, e.Subtree})
				}
				if err := a.wrote(); err != nil {
					return err
				}
				continue
			case o.seen:
				continue // a name twice in one folder
			}
			o.seen = true
			wasDir := o.kind == 1
			if o.last.Valid {
				// It went away and is back.
				if o.last.Int64 >= o.first {
					if _, err := st.past.Exec(o.id, o.first, o.last.Int64); err != nil {
						return err
					}
				}
				if _, err := st.reopen.Exec(a.seq, o.id); err != nil {
					return err
				}
				o.sub.Valid = false
				if err := a.wrote(); err != nil {
					return err
				}
			} else if wasDir && kind != 1 {
				if err := a.closeBelow(o.id); err != nil {
					return err
				}
			}
			if o.kind != kind || o.size != size || o.mtime != mtime {
				if _, err := st.update.Exec(kind, size, mtime, o.id); err != nil {
					return err
				}
				if err := a.wrote(); err != nil {
					return err
				}
			}
			if kind == 1 && !(wasDir && o.sub.Valid && o.sub.Int64 == subHash(e.Subtree)) {
				if o.sub.Valid {
					// Half read from here on, until it is read through.
					if _, err := st.setSub.Exec(nil, o.id); err != nil {
						return err
					}
				}
				below = append(below, todo{o.id, e.Subtree})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, o := range olds {
		if o.seen || o.last.Valid {
			continue
		}
		if _, err := st.closeOne.Exec(a.prev, o.id); err != nil {
			return err
		}
		if o.kind == 1 {
			if err := a.closeBelow(o.id); err != nil {
				return err
			}
		}
		if err := a.wrote(); err != nil {
			return err
		}
	}
	olds = nil
	for _, t := range below {
		err := a.dir(t.id, func(each func([]entry) error) error {
			for _, p := range t.pages {
				b, err := a.get(p)
				if err != nil {
					return err
				}
				var es []entry
				if err := json.Unmarshal(b, &es); err != nil {
					return fmt.Errorf("directory page %s: %w", p.Short(), err)
				}
				if err := each(es); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := st.setSub.Exec(subHash(t.pages), t.id); err != nil {
			return err
		}
		if err := a.wrote(); err != nil {
			return err
		}
	}
	return nil
}

// closeBelow marks everything under a folder as gone since the
// snapshot before this one.
func (a *adder) closeBelow(id int64) error {
	st := &a.ix.st
	stack := []int64{id}
	for len(stack) > 0 {
		id, stack = stack[len(stack)-1], stack[:len(stack)-1]
		rows, err := st.dirKids.Query(id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var kid int64
			rows.Scan(&kid)
			stack = append(stack, kid)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if _, err := st.closeKids.Exec(a.prev, id); err != nil {
			return err
		}
		if err := a.wrote(); err != nil {
			return err
		}
	}
	return nil
}

// Searching.

// Query is what to look for.
type Query struct {
	// Text is what was typed: part of a name, or a pattern with * and ?.
	Text string
	// Match says whether a name is wanted. The index narrows the names
	// down with Text and Match has the last word.
	Match func(name string) bool
	// Agent, if set, keeps the search to one agent.
	Agent string
	// Limit is the most hits to return.
	Limit int
	// Timeout stops a search that has to read every name.
	Timeout time.Duration
}

// Hit is a path whose name matched.
type Hit struct {
	// Path is the name of the source, then the names below it.
	Path    []string
	Type    string
	Size    int64
	ModTime time.Time
	// Newest and Oldest are the newest and oldest snapshots that hold
	// the path, and Snapshots how many do.
	Newest, Oldest string
	Snapshots      int
}

type Result struct {
	Hits []Hit
	// Snapshots is how many snapshots were searched.
	Snapshots int
	// More says there are more hits than Limit; Partial that the
	// search ran out of time.
	More, Partial bool
}

// lines returns the snapshots that exist and are indexed, by series.
func (ix *Index) lines() (map[int64]*line, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.live != nil {
		return ix.live, nil
	}
	live := map[int64]*line{}
	rows, err := ix.r.Query(`SELECT id, agent FROM series`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		l := new(line)
		rows.Scan(&id, &l.agent)
		live[id] = l
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = ix.r.Query(`SELECT seq, id, series FROM snaps WHERE done = 1 AND gone = 0 ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var seq, series int64
		var id string
		rows.Scan(&seq, &id, &series)
		if l := live[series]; l != nil {
			l.seqs, l.ids = append(l.seqs, seq), append(l.ids, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	ix.live = live
	return live, nil
}

// within returns the places in l of the snapshots numbered first to
// last: from lo up to, not including, hi.
func (l *line) within(first int64, last sql.NullInt64) (lo, hi int) {
	lo = sort.Search(len(l.seqs), func(i int) bool { return l.seqs[i] >= first })
	hi = len(l.seqs)
	if last.Valid {
		hi = sort.Search(len(l.seqs), func(i int) bool { return l.seqs[i] > last.Int64 })
	}
	return lo, max(lo, hi)
}

// trigrams returns a full-text query for the names that can match
// text, or "" if text has no run of three letters to look up. The
// index of names ignores case by itself.
func trigrams(text string) string {
	var terms []string
	seen := map[string]bool{}
	for _, run := range strings.FieldsFunc(text, func(r rune) bool { return r == '*' || r == '?' }) {
		rs := []rune(run)
		for i := 0; i+3 <= len(rs) && len(terms) < 16; i++ {
			t := string(rs[i : i+3])
			if strings.ContainsRune(t, 0) || seen[t] {
				continue
			}
			seen[t] = true
			terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
		}
	}
	return strings.Join(terms, " AND ")
}

// Search finds the paths whose names match.
func (ix *Index) Search(ctx context.Context, q Query) (*Result, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	if q.Timeout <= 0 {
		q.Timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, q.Timeout)
	defer cancel()
	live, err := ix.lines()
	if err != nil {
		return nil, err
	}
	out := &Result{}
	for _, l := range live {
		if q.Agent == "" || l.agent == q.Agent {
			out.Snapshots += len(l.seqs)
		}
	}
	const cols = `n.id, n.parent, n.name, n.series, n.kind, n.size, n.mtime, n.first, n.last`
	var rows *sql.Rows
	if m := trigrams(q.Text); m != "" {
		rows, err = ix.r.QueryContext(ctx, `SELECT `+cols+` FROM names f JOIN nodes n ON n.id = f.rowid WHERE f.names MATCH ? ORDER BY f.rowid DESC`, m)
	} else {
		// Too short for the index of names: read them all, newest first.
		rows, err = ix.r.QueryContext(ctx, `SELECT `+cols+` FROM nodes n ORDER BY n.id DESC`)
	}
	if err != nil {
		return nil, err
	}
	type cand struct {
		id, parent, series, first int64
		last                      sql.NullInt64
		name                      string
		hit                       Hit
		newest                    int64
	}
	var cands []cand
	// Gather some more than are asked for, so that the newest are
	// shown when there are too many.
	for rows.Next() {
		var c cand
		var mtime int64
		var kind int
		if err := rows.Scan(&c.id, &c.parent, &c.name, &c.series, &kind, &c.hit.Size, &mtime, &c.first, &c.last); err != nil {
			rows.Close()
			return nil, err
		}
		l := live[c.series]
		name := c.name
		if c.parent < 0 {
			// A source is named by its whole path.
			name = strings.TrimRight(name, `/\`)
			name = name[strings.LastIndexAny(name, `/\`)+1:]
		}
		if l == nil || len(l.seqs) == 0 || (q.Agent != "" && l.agent != q.Agent) || !q.Match(name) {
			continue
		}
		if kind >= 0 && kind < len(kinds) {
			c.hit.Type = kinds[kind]
		}
		c.hit.ModTime = time.Unix(mtime, 0)
		cands = append(cands, c)
		if len(cands) >= 5*q.Limit {
			out.More = true
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		if ctx.Err() == nil {
			return nil, err
		}
		out.More, out.Partial = true, true
	}
	// The connection is free again: now find where and when each was.
	ctx = context.WithoutCancel(ctx)
	type up struct {
		parent int64
		name   string
	}
	ups := map[int64]up{}
	for i := range cands {
		c := &cands[i]
		l := live[c.series]
		lo, hi := l.within(c.first, c.last)
		n, oldest, newest := hi-lo, lo, hi-1
		rows, err := ix.r.QueryContext(ctx, `SELECT first, last FROM past WHERE node = ?`, c.id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var first int64
			var last sql.NullInt64
			rows.Scan(&first, &last)
			plo, phi := l.within(first, last)
			if phi == plo {
				continue
			}
			if n == 0 {
				newest = phi - 1
			}
			n, oldest = n+phi-plo, min(oldest, plo)
			newest = max(newest, phi-1)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if n == 0 {
			continue // only forgotten snapshots held it
		}
		c.hit.Snapshots, c.hit.Oldest, c.hit.Newest, c.newest = n, l.ids[oldest], l.ids[newest], l.seqs[newest]
		path := []string{c.name}
		for at := c.parent; at > 0; {
			u, ok := ups[at]
			if !ok {
				if err := ix.r.QueryRowContext(ctx, `SELECT parent, name FROM nodes WHERE id = ?`, at).Scan(&u.parent, &u.name); err != nil {
					return nil, err
				}
				ups[at] = u
			}
			path = append(path, u.name)
			at = u.parent
		}
		for a, b := 0, len(path)-1; a < b; a, b = a+1, b-1 {
			path[a], path[b] = path[b], path[a]
		}
		c.hit.Path = path
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].newest > cands[j].newest })
	for i := range cands {
		if cands[i].hit.Snapshots == 0 {
			continue
		}
		if len(out.Hits) >= q.Limit {
			out.More = true
			break
		}
		out.Hits = append(out.Hits, cands[i].hit)
	}
	return out, nil
}
