package repo

import (
	"fmt"
	"os"

	"github.com/ajthom90/better-backup/internal/keys"
)

// GCStats reports what a garbage collection did.
type GCStats struct {
	PacksDeleted  int64 `json:"packs_deleted"`
	PacksRepacked int64 `json:"packs_repacked"`
	BytesFreed    int64 `json:"bytes_freed"`
	LiveChunks    int64 `json:"live_chunks"`
}

// GC deletes data no snapshot refers to. walk must call mark for every
// live chunk. Packs with no live chunks are deleted; packs less than half
// live are rewritten. Nothing is deleted if any live chunk is missing,
// since that means the walk or the repository is not trustworthy.
//
// The caller must ensure no backup or restore runs concurrently.
func (r *Repo) GC(walk func(mark func(keys.ID) error) error) (GCStats, error) {
	var st GCStats
	if err := r.Flush(); err != nil {
		return st, err
	}
	if _, err := r.db.Exec(`DROP TABLE IF EXISTS gc_live; CREATE TABLE gc_live (id BLOB PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return st, err
	}
	defer r.db.Exec(`DROP TABLE IF EXISTS gc_live`)

	var batch []keys.ID
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		ins, err := tx.Prepare(`INSERT OR IGNORE INTO gc_live (id) VALUES (?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, id := range batch {
			if _, err := ins.Exec(id[:]); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return tx.Commit()
	}
	err := walk(func(id keys.ID) error {
		batch = append(batch, id)
		if len(batch) >= 4096 {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return st, fmt.Errorf("gc: marking live data: %w", err)
	}

	var missing int64
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM gc_live`).Scan(&st.LiveChunks); err != nil {
		return st, err
	}
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM gc_live l LEFT JOIN chunks c ON c.id = l.id WHERE c.id IS NULL`).Scan(&missing); err != nil {
		return st, err
	}
	if missing > 0 {
		return st, fmt.Errorf("gc aborted: %d chunks referenced by snapshots are missing from the repository; run verify", missing)
	}

	type packInfo struct {
		id         packID
		size, live int64
	}
	var dead, sparse []packInfo
	rows, err := r.db.Query(`SELECT p.id, p.size, COALESCE((SELECT SUM(c.len) FROM chunks c JOIN gc_live l ON l.id = c.id WHERE c.pack = p.id), 0) FROM packs p`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var b []byte
		var p packInfo
		if err := rows.Scan(&b, &p.size, &p.live); err != nil {
			rows.Close()
			return st, err
		}
		p.id = packID(b)
		switch {
		case p.live == 0:
			dead = append(dead, p)
		case p.live*2 < p.size:
			sparse = append(sparse, p)
		}
	}
	if err := rows.Close(); err != nil {
		return st, err
	}

	// Copy live chunks out of sparse packs. Chunks are encrypted under a
	// key derived from their own id, so they move without re-encryption.
	for _, p := range sparse {
		entries, err := r.liveEntries(p.id)
		if err != nil {
			return st, err
		}
		f, err := os.Open(r.packPath(p.id))
		if err != nil {
			return st, err
		}
		for _, e := range entries {
			buf := make([]byte, e.length)
			if _, err := f.ReadAt(buf, e.off); err != nil {
				f.Close()
				return st, fmt.Errorf("gc: read pack %s: %w", p.id, err)
			}
			if _, err := keys.Open(&r.keys.Enc, e.id[:], buf); err != nil {
				f.Close()
				return st, fmt.Errorf("gc aborted: live chunk %s in pack %s is corrupt", e.id.Short(), p.id)
			}
			r.mu.Lock()
			err := r.appendLocked(e.id, buf, e.rawLen)
			r.mu.Unlock()
			if err != nil {
				f.Close()
				return st, err
			}
		}
		f.Close()
		st.BytesFreed += p.size - p.live
	}
	// The new packs are durable and indexed before any old pack goes.
	if err := r.Flush(); err != nil {
		return st, err
	}
	for _, p := range append(dead, sparse...) {
		tx, err := r.db.Begin()
		if err != nil {
			return st, err
		}
		_, err = tx.Exec(`DELETE FROM chunks WHERE pack = ?`, p.id[:])
		if err == nil {
			_, err = tx.Exec(`DELETE FROM packs WHERE id = ?`, p.id[:])
		}
		if err != nil {
			tx.Rollback()
			return st, err
		}
		if err := tx.Commit(); err != nil {
			return st, err
		}
		if err := os.Remove(r.packPath(p.id)); err != nil && !os.IsNotExist(err) {
			return st, err
		}
	}
	for _, p := range dead {
		st.BytesFreed += p.size
	}
	st.PacksDeleted, st.PacksRepacked = int64(len(dead)), int64(len(sparse))
	return st, nil
}

func (r *Repo) liveEntries(p packID) ([]entry, error) {
	rows, err := r.db.Query(`SELECT c.id, c.off, c.len, c.rawlen FROM chunks c JOIN gc_live l ON l.id = c.id WHERE c.pack = ? ORDER BY c.off`, p[:])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var b []byte
		var e entry
		if err := rows.Scan(&b, &e.off, &e.length, &e.rawLen); err != nil {
			return nil, err
		}
		e.id = keys.ID(b)
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyStats reports the result of a full repository check.
type VerifyStats struct {
	Packs  int64    `json:"packs"`
	Chunks int64    `json:"chunks"`
	Bytes  int64    `json:"bytes"`
	Errors []string `json:"errors"`
}

// Verify reads every pack, decrypts and decompresses every chunk and
// checks it against its id and the index.
func (r *Repo) Verify() (VerifyStats, error) {
	st := VerifyStats{Errors: []string{}}
	if err := r.Flush(); err != nil {
		return st, err
	}
	fail := func(format string, a ...any) {
		if len(st.Errors) < 100 {
			st.Errors = append(st.Errors, fmt.Sprintf(format, a...))
		}
	}
	rows, err := r.db.Query(`SELECT id FROM packs`)
	if err != nil {
		return st, err
	}
	var packs []packID
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return st, err
		}
		packs = append(packs, packID(b))
	}
	if err := rows.Close(); err != nil {
		return st, err
	}
	for _, p := range packs {
		st.Packs++
		path := r.packPath(p)
		entries, _, err := r.readTrailer(path, p)
		if err != nil {
			fail("pack %s: %v", p, err)
			continue
		}
		var indexed int64
		if err := r.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE pack = ?`, p[:]).Scan(&indexed); err != nil {
			return st, err
		}
		if indexed > int64(len(entries)) {
			fail("pack %s: index lists %d chunks but the pack holds %d", p, indexed, len(entries))
		}
		f, err := os.Open(path)
		if err != nil {
			fail("pack %s: %v", p, err)
			continue
		}
		for _, e := range entries {
			buf := make([]byte, e.length)
			if _, err := f.ReadAt(buf, e.off); err != nil {
				fail("pack %s: chunk %s: %v", p, e.id.Short(), err)
				continue
			}
			data, err := r.unseal(e.id, buf)
			if err != nil {
				fail("pack %s: %v", p, err)
				continue
			}
			if keys.ChunkID(&r.keys.ID, data) != e.id {
				fail("pack %s: chunk %s does not match its id", p, e.id.Short())
				continue
			}
			st.Chunks++
			st.Bytes += int64(len(data))
		}
		f.Close()
	}
	return st, nil
}
