package repo

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ajthom90/better-backup/internal/keys"
)

func openTest(t *testing.T, dir string) *Repo {
	t.Helper()
	if !Exists(dir) {
		if err := Init(dir, []byte("pw")); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(dir, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	r.PackSize = 256 << 10
	t.Cleanup(func() { r.Close() })
	return r
}

func blob(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestPutGetDedupReopen(t *testing.T) {
	dir := t.TempDir()
	r := openTest(t, dir)
	var ids []keys.ID
	for i := 0; i < 40; i++ {
		data := blob(int64(i), 50_000)
		if i%2 == 0 {
			data = bytes.Repeat([]byte("compressible "), 4000) // identical: dedups
		}
		id, err := r.PutData(data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		got, err := r.Get(id) // may still be in the open pack
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("get pending chunk %d: %v", i, err)
		}
	}
	st, _ := r.Stats()
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	st, _ = r.Stats()
	if st.Chunks != 21 {
		t.Fatalf("want 21 unique chunks, got %d", st.Chunks)
	}
	if err := r.Put(ids[1], []byte("wrong content")); err == nil {
		t.Fatal("Put accepted a chunk that does not match its id")
	}
	r.Close()

	if _, err := Open(dir, []byte("bad")); err == nil {
		t.Fatal("opened with the wrong passphrase")
	}
	// Losing the index must not lose data.
	for _, f := range []string{"index.db", "index.db-wal", "index.db-shm"} {
		os.Remove(filepath.Join(dir, f))
	}
	r = openTest(t, dir)
	for i, id := range ids {
		got, err := r.Get(id)
		if err != nil {
			t.Fatalf("chunk %d after index rebuild: %v", i, err)
		}
		if i%2 == 1 && !bytes.Equal(got, blob(int64(i), 50_000)) {
			t.Fatalf("chunk %d content differs", i)
		}
	}
	v, err := r.Verify()
	if err != nil || len(v.Errors) != 0 || v.Chunks != 21 {
		t.Fatalf("verify: %+v %v", v, err)
	}
}

func TestGC(t *testing.T) {
	r := openTest(t, t.TempDir())
	var ids []keys.ID
	for i := 0; i < 60; i++ {
		id, err := r.PutData(blob(int64(i), 40_000))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	before, _ := r.Stats()
	live := map[keys.ID]bool{}
	for i, id := range ids {
		if i%5 == 0 || i >= 50 { // sparse survivors plus a fully live tail
			live[id] = true
		}
	}
	st, err := r.GC(func(mark func(keys.ID) error) error {
		for id := range live {
			if err := mark(id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := r.Stats()
	if st.PacksRepacked == 0 || after.StoredBytes >= before.StoredBytes/2 {
		t.Fatalf("gc reclaimed too little: %+v before=%+v after=%+v", st, before, after)
	}
	for i, id := range ids {
		data, err := r.Get(id)
		if live[id] && (err != nil || !bytes.Equal(data, blob(int64(i), 40_000))) {
			t.Fatalf("live chunk %d lost: %v", i, err)
		}
	}
	if v, err := r.Verify(); err != nil || len(v.Errors) != 0 {
		t.Fatalf("verify after gc: %+v %v", v, err)
	}
	// A walk that names a chunk the repository lacks must delete nothing.
	if _, err := r.GC(func(mark func(keys.ID) error) error { return mark(keys.ID{1}) }); err == nil {
		t.Fatal("gc ran with a missing live chunk")
	}
	if now, _ := r.Stats(); now != after {
		t.Fatal("aborted gc changed the repository")
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	r := openTest(t, dir)
	for i := 0; i < 5; i++ {
		r.PutData(blob(int64(i), 30_000))
	}
	r.Flush()
	packs, _ := filepath.Glob(filepath.Join(dir, "packs", "*", "*.pack"))
	b, _ := os.ReadFile(packs[0])
	b[100] ^= 0xff
	os.WriteFile(packs[0], b, 0o600)
	if v, _ := r.Verify(); len(v.Errors) == 0 {
		t.Fatal("verify missed a flipped byte")
	}
}
