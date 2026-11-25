package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestSignature checks the signer against the worked examples in the
// Amazon S3 documentation ("Signature Calculations for the Authorization
// Header").
func TestSignature(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	headers := map[string]string{"host": "examplebucket.s3.amazonaws.com", "x-amz-date": "20130524T000000Z", "x-amz-content-sha256": emptyHash}
	const ak, sk = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	got := sign("GET", "/", url.Values{"max-keys": {"2"}, "prefix": {"J"}}, headers, emptyHash, ak, sk, "us-east-1", at)
	if want := "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"; !strings.HasSuffix(got, want) {
		t.Errorf("list objects: %s", got)
	}
	got = sign("GET", "/", url.Values{"lifecycle": {""}}, headers, emptyHash, ak, sk, "us-east-1", at)
	if want := "Signature=fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"; !strings.HasSuffix(got, want) {
		t.Errorf("bucket lifecycle: %s", got)
	}
	headers["range"] = "bytes=0-9"
	got = sign("GET", "/test.txt", nil, headers, emptyHash, ak, sk, "us-east-1", at)
	if want := "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; !strings.HasSuffix(got, want) {
		t.Errorf("get object: %s", got)
	}
}

// fakeS3 is a small in-memory S3 server. It recomputes every request's
// signature from what arrived on the wire, checks the payload hash, and
// pages listings two keys at a time. With lock set it is a bucket with
// Object Lock: it keeps every version, deleting puts a marker on top,
// and a locked version cannot be removed.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	ak, sk  string
	fail    int // fail this many PUTs with a 500 first

	lock     bool
	versions map[string][]*fakeVersion // newest first
	nextID   int
}

type fakeVersion struct {
	id     string
	data   []byte
	marker bool
	at     time.Time
	until  time.Time
}

// current returns the newest version of a key if it is not a marker.
func (f *fakeS3) current(key string) *fakeVersion {
	if vs := f.versions[key]; len(vs) > 0 && !vs[0].marker {
		return vs[0]
	}
	return nil
}

func (f *fakeS3) push(key string, v *fakeVersion) {
	f.nextID++
	v.id, v.at = fmt.Sprintf("v%06d", f.nextID), time.Now()
	f.versions[key] = append([]*fakeVersion{v}, f.versions[key]...)
	if v.marker {
		delete(f.objects, key)
	} else {
		f.objects[key] = v.data
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	deny := func(msg string) {
		w.WriteHeader(403)
		fmt.Fprintf(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>%s</Message></Error>", msg)
	}
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		deny("payload hash")
		return
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		deny("date")
		return
	}
	headers := map[string]string{"host": r.Host, "x-amz-date": r.Header.Get("X-Amz-Date"), "x-amz-content-sha256": r.Header.Get("X-Amz-Content-Sha256")}
	for _, h := range []string{"X-Amz-Object-Lock-Mode", "X-Amz-Object-Lock-Retain-Until-Date"} {
		if v := r.Header.Get(h); v != "" {
			headers[strings.ToLower(h)] = v
		}
	}
	if want := sign(r.Method, r.URL.EscapedPath(), r.URL.Query(), headers, headers["x-amz-content-sha256"], f.ak, f.sk, "test-region", at); want != r.Header.Get("Authorization") {
		deny("signature")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if parts[0] != "bucket" {
		w.WriteHeader(404)
		fmt.Fprint(w, "<Error><Code>NoSuchBucket</Code><Message>no such bucket</Message></Error>")
		return
	}
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}
	q := r.URL.Query()
	refuse := func(code, msg string) {
		w.WriteHeader(403)
		fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
	}
	switch {
	case r.Method == "GET" && key == "" && q.Has("object-lock"):
		if !f.lock {
			w.WriteHeader(404)
			fmt.Fprint(w, "<Error><Code>ObjectLockConfigurationNotFoundError</Code><Message>Object Lock configuration does not exist for this bucket</Message></Error>")
			return
		}
		fmt.Fprint(w, "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>")
	case r.Method == "GET" && key == "" && q.Has("versions"):
		type item struct {
			key string
			v   *fakeVersion
			top bool
		}
		var all []item
		var keys []string
		for k := range f.versions {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			for i, v := range f.versions[k] {
				all = append(all, item{k, v, i == 0})
			}
		}
		start := 0
		if km := q.Get("key-marker"); km != "" {
			for i, it := range all {
				if it.key == km && it.v.id == q.Get("version-id-marker") {
					start = i + 1
				}
			}
		}
		all = all[start:]
		fmt.Fprint(w, "<ListVersionsResult><Name>bucket</Name>")
		if len(all) > 3 {
			all = all[:3]
			fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextVersionIdMarker>%s</NextVersionIdMarker>", all[2].key, all[2].v.id)
		}
		for _, it := range all {
			tag := "Version"
			if it.v.marker {
				tag = "DeleteMarker"
			}
			fmt.Fprintf(w, "<%s><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%v</IsLatest><LastModified>%s</LastModified><Size>%d</Size></%s>",
				tag, it.key, it.v.id, it.top, it.v.at.UTC().Format("2006-01-02T15:04:05.000000Z"), len(it.v.data), tag)
		}
		fmt.Fprint(w, "</ListVersionsResult>")
	case r.Method == "GET" && key == "":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("continuation-token") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, "<ListBucketResult>")
		if len(keys) > 2 {
			keys = keys[:2]
			fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", keys[1])
		}
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(f.objects[k]))
		}
		fmt.Fprint(w, "</ListBucketResult>")
	case r.Method == "PUT" && q.Has("retention"):
		v := f.current(key)
		var ret struct {
			Until time.Time `xml:"RetainUntilDate"`
		}
		if v == nil || xml.Unmarshal(body, &ret) != nil || r.Header.Get("Content-MD5") == "" {
			w.WriteHeader(400)
			return
		}
		if ret.Until.Before(v.until) {
			refuse("AccessDenied", "the lock cannot be shortened")
			return
		}
		v.until = ret.Until
	case r.Method == "PUT":
		if f.fail > 0 {
			f.fail--
			w.WriteHeader(500)
			return
		}
		if !f.lock {
			f.objects[key] = body
			return
		}
		v := &fakeVersion{data: body}
		if d := r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"); d != "" {
			if v.until, err = time.Parse(time.RFC3339, d); err != nil || r.Header.Get("Content-MD5") == "" || r.Header.Get("X-Amz-Object-Lock-Mode") == "" {
				w.WriteHeader(400)
				return
			}
		}
		f.push(key, v)
	case r.Method == "GET" || r.Method == "HEAD":
		b, ok := f.objects[key]
		if id := q.Get("versionId"); id != "" {
			ok = false
			for _, v := range f.versions[key] {
				if v.id == id && !v.marker {
					b, ok = v.data, true
				}
			}
		} else if v := f.current(key); f.lock && v != nil && !v.until.IsZero() {
			w.Header().Set("X-Amz-Object-Lock-Retain-Until-Date", v.until.UTC().Format(time.RFC3339))
		}
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	case r.Method == "DELETE" && q.Get("versionId") != "":
		vs := f.versions[key]
		for i, v := range vs {
			if v.id != q.Get("versionId") {
				continue
			}
			if v.until.After(time.Now()) {
				refuse("AccessDenied", "Object is WORM protected and cannot be overwritten")
				return
			}
			f.versions[key] = append(vs[:i:i], vs[i+1:]...)
			if len(f.versions[key]) == 0 {
				delete(f.versions, key)
			}
			delete(f.objects, key)
			if c := f.current(key); c != nil {
				f.objects[key] = c.data
			}
		}
		w.WriteHeader(204)
	case r.Method == "DELETE":
		if f.lock {
			f.push(key, &fakeVersion{marker: true})
		} else {
			delete(f.objects, key)
		}
		w.WriteHeader(204)
	}
}

func newRepo(t *testing.T) (string, *repo.Repo) {
	t.Helper()
	dir := t.TempDir()
	if err := repo.Init(dir, []byte("pw")); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(dir, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	r.PackSize = 256 << 10
	t.Cleanup(func() { r.Close() })
	return dir, r
}

func fill(t *testing.T, r *repo.Repo, seed int64, snap string) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 12; i++ {
		b := make([]byte, 100<<10)
		rng.Read(b)
		if _, err := r.PutData(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveSnapshot(&tree.Snapshot{ID: snap}); err != nil {
		t.Fatal(err)
	}
}

func targets(t *testing.T) map[string]Target {
	fake := &fakeS3{objects: map[string][]byte{}, ak: "key", sk: "secret", fail: 1}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	s3, err := NewS3(S3Config{Endpoint: srv.URL, Region: "test-region", Bucket: "bucket", Prefix: "site a/copy", AccessKey: "key", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Target{"s3": s3, "dir": dir}
}

func TestSyncAndFetch(t *testing.T) {
	ctx := context.Background()
	for name, target := range targets(t) {
		t.Run(name, func(t *testing.T) {
			dir, r := newRepo(t)
			fill(t, r, 1, "aaaa")
			res, err := Sync(ctx, dir, target, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Packs, one snapshot, and the config.
			if res.Uploaded < 4 || res.Objects != res.Uploaded+1 || res.Deleted != 0 {
				t.Fatalf("first sync: %+v", res)
			}
			// Nothing changed: nothing is sent.
			res2, err := Sync(ctx, dir, target, nil, nil)
			if err != nil || res2.Uploaded != 0 || res2.Objects != res.Objects || res2.Bytes != res.Bytes {
				t.Fatalf("second sync: %+v %v", res2, err)
			}
			// More data and a forgotten snapshot.
			fill(t, r, 2, "bbbb")
			if err := r.DeleteSnapshot("aaaa"); err != nil {
				t.Fatal(err)
			}
			res3, err := Sync(ctx, dir, target, nil, nil)
			if err != nil || res3.Uploaded < 4 || res3.Deleted != 1 {
				t.Fatalf("third sync: %+v %v", res3, err)
			}

			// The copy opens as a repository and holds the same data.
			out := filepath.Join(t.TempDir(), "restored")
			if err := Fetch(ctx, target, out, nil); err != nil {
				t.Fatal(err)
			}
			c, err := repo.OpenReadOnly(out, []byte("pw"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			snaps, err := c.LoadSnapshots()
			if err != nil || len(snaps) != 1 || snaps[0].ID != "bbbb" {
				t.Fatalf("snapshots in the copy: %v %v", snaps, err)
			}
			if st, err := c.Verify(); err != nil || len(st.Errors) != 0 || st.Chunks != 24 {
				t.Fatalf("verify of the copy: %+v %v", st, err)
			}

			// A different repository must not be mixed in.
			other, r2 := newRepo(t)
			fill(t, r2, 3, "cccc")
			if _, err := Sync(ctx, other, target, nil, nil); err == nil || !strings.Contains(err.Error(), "different repository") {
				t.Fatalf("sync of another repository: %v", err)
			}
		})
	}
}

// TestSyncKeepsCopyOfLostRepository: when most of the repository has
// vanished, the copy is the only good one and must not be emptied.
func TestSyncKeepsCopyOfLostRepository(t *testing.T) {
	ctx := context.Background()
	dir, r := newRepo(t)
	for i := 0; i < 8; i++ {
		fill(t, r, int64(i), fmt.Sprintf("s%03d", i))
	}
	target, _ := NewDir(t.TempDir())
	res, err := Sync(ctx, dir, target, nil, nil)
	if err != nil || res.Objects < 30 {
		t.Fatalf("%+v %v", res, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "packs")); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, dir, target, nil, nil); err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("sync after losing the packs: %v", err)
	}
	left, _ := target.List(ctx)
	if len(left) != res.Objects {
		t.Fatalf("the copy went from %d to %d files", res.Objects, len(left))
	}
}

func TestDirTargetNeedsFolder(t *testing.T) {
	target, _ := NewDir(filepath.Join(t.TempDir(), "not-mounted"))
	if _, err := target.List(context.Background()); err == nil {
		t.Fatal("a missing folder was accepted")
	}
	if err := target.Put(context.Background(), "config.json", bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("a missing folder was created")
	}
}

// TestRealS3 runs the same round trip against a real S3-compatible
// service. It is skipped unless BB_TEST_S3_ENDPOINT,
// BB_TEST_S3_ACCESS_KEY and BB_TEST_S3_SECRET_KEY are set; the keys must
// be allowed to create a bucket.
func TestRealS3(t *testing.T) {
	endpoint := os.Getenv("BB_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("BB_TEST_S3_ENDPOINT is not set")
	}
	ctx := context.Background()
	cfg := S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: fmt.Sprintf("bb-test-%d", time.Now().UnixNano()),
		Prefix: "copies/site a", AccessKey: os.Getenv("BB_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("BB_TEST_S3_SECRET_KEY")}
	target, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A bucket that does not exist is reported, not created.
	if _, err := target.List(ctx); err == nil {
		t.Fatal("listing a missing bucket worked")
	}
	s3 := target.(*s3Target)
	prefix := s3.prefix
	s3.prefix = ""
	resp, err := s3.do(ctx, "PUT", "", nil, nil, 0) // create the bucket
	s3.prefix = prefix
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	dir, r := newRepo(t)
	for i := 0; i < 5; i++ {
		fill(t, r, int64(i), fmt.Sprintf("s%03d", i))
	}
	res, err := Sync(ctx, dir, target, nil, nil)
	if err != nil || res.Uploaded < 20 {
		t.Fatalf("sync: %+v %v", res, err)
	}
	listed, err := target.List(ctx)
	if err != nil || len(listed) != res.Objects {
		t.Fatalf("listed %d objects, synced %d: %v", len(listed), res.Objects, err)
	}
	if err := r.DeleteSnapshot("s000"); err != nil {
		t.Fatal(err)
	}
	if res, err = Sync(ctx, dir, target, nil, nil); err != nil || res.Uploaded != 0 || res.Deleted != 1 {
		t.Fatalf("second sync: %+v %v", res, err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if err := Fetch(ctx, target, out, nil); err != nil {
		t.Fatal(err)
	}
	c, err := repo.OpenReadOnly(out, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st, err := c.Verify(); err != nil || len(st.Errors) != 0 || st.Chunks != 60 {
		t.Fatalf("verify of the copy: %+v %v", st, err)
	}
	// A wrong secret key is refused with the service's reason.
	cfg.SecretKey = "wrong"
	bad, _ := NewS3(cfg)
	if _, err := bad.List(ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("wrong secret key: %v", err)
	}
}

// lockedCopy does to a locked copy what an intruder with the server's
// own storage keys would, and checks that the copy survives it.
func lockedCopy(t *testing.T, target Target) {
	ctx := context.Background()
	s3 := target.(*s3Target)
	dir, r := newRepo(t)
	for i := 0; i < 3; i++ {
		fill(t, r, int64(i), fmt.Sprintf("s%03d", i))
	}
	lock := &Lock{Days: 30, Mode: LockCompliance}
	res, err := Sync(ctx, dir, target, lock, nil)
	if err != nil || res.Uploaded < 12 || res.Relocked != 0 || res.LockedUntil == nil {
		t.Fatalf("locked sync: %+v %v", res, err)
	}
	if d := time.Until(*res.LockedUntil); d < 30*24*time.Hour || d > 41*24*time.Hour {
		t.Fatalf("locked until %s", res.LockedUntil)
	}
	objects := res.Objects

	// A run that does not know the date of the last one moves every
	// lock, which the service takes although nothing changes; a run
	// that knows it leaves them alone.
	if res, err = Sync(ctx, dir, target, lock, nil); err != nil || res.Uploaded != 0 || res.Relocked != objects {
		t.Fatalf("second locked sync: %+v %v (%d objects)", res, err, objects)
	}
	lock.Until = *res.LockedUntil
	if res, err = Sync(ctx, dir, target, lock, nil); err != nil || res.Relocked != 0 || res.Pruned != 0 {
		t.Fatalf("third locked sync: %+v %v", res, err)
	}
	// A shorter lock is not an error: what is there keeps its date.
	if err := s3.Relock(ctx, "config.json", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("asking for a shorter lock: %v", err)
	}

	// Forgetting a snapshot hides it in the copy, and nothing more.
	if err := r.DeleteSnapshot("s000"); err != nil {
		t.Fatal(err)
	}
	if res, err = Sync(ctx, dir, target, lock, nil); err != nil || res.Deleted != 1 || res.Pruned != 0 {
		t.Fatalf("sync after forgetting: %+v %v", res, err)
	}
	time.Sleep(1500 * time.Millisecond)
	before := time.Now()
	time.Sleep(1500 * time.Millisecond)

	// The attack: delete everything, overwrite the config, and try to
	// remove the versions underneath.
	listed, err := target.List(ctx)
	if err != nil || len(listed) != objects-1 {
		t.Fatalf("listed %d objects, want %d: %v", len(listed), objects-1, err)
	}
	for _, o := range listed {
		if err := target.Delete(ctx, o.Name); err != nil {
			t.Fatal(err)
		}
	}
	s3.SetLock("", time.Time{})
	if err := target.Put(ctx, "config.json", bytes.NewReader([]byte("{}")), 2); err != nil {
		t.Fatal(err)
	}
	all, err := s3.listVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked := 0
	for _, v := range all {
		if v.marker() || v.Size == 2 {
			continue
		}
		locked++
		if resp, err := s3.do(ctx, "DELETE", v.Key, url.Values{"versionId": {v.ID}}, nil, 0); err == nil {
			resp.Body.Close()
			t.Fatalf("a locked version of %s was deleted", v.Key)
		}
	}
	if locked < objects {
		t.Fatalf("%d locked versions, want at least %d", locked, objects)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if err := Fetch(ctx, target, gone, nil); err == nil {
		if c, err := repo.OpenReadOnly(gone, []byte("pw")); err == nil {
			c.Close()
			t.Fatal("the copy as it is now still opens after everything was deleted")
		}
	}

	// The copy as it was before the attack is whole.
	out := filepath.Join(t.TempDir(), "restored")
	if err := Fetch(ctx, target.(Locker).AsOf(before), out, nil); err != nil {
		t.Fatal(err)
	}
	c, err := repo.OpenReadOnly(out, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	snaps, err := c.LoadSnapshots()
	if st, verr := c.Verify(); err != nil || verr != nil || len(st.Errors) != 0 || st.Chunks != 36 || len(snaps) != 2 {
		t.Fatalf("the copy from before the attack: %+v, %d snapshots, %v %v", st, len(snaps), err, verr)
	}
	if err := target.(Locker).AsOf(before).Put(ctx, "x", bytes.NewReader(nil), 0); err == nil {
		t.Fatal("a view of the past took a write")
	}

	// Pruning removes what is no longer current and no longer locked:
	// here the overwritten config and the markers with nothing under
	// them. Everything locked stays.
	n, err := s3.Prune(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	after, err := s3.listVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	left := 0
	for _, v := range after {
		if !v.marker() && v.Size != 2 {
			left++
		}
	}
	if left != locked {
		t.Fatalf("pruning removed locked versions: %d left of %d (removed %d)", left, locked, n)
	}
}

func TestObjectLock(t *testing.T) {
	ctx := context.Background()
	fake := &fakeS3{objects: map[string][]byte{}, versions: map[string][]*fakeVersion{}, ak: "key", sk: "secret"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	target, err := NewS3(S3Config{Endpoint: srv.URL, Region: "test-region", Bucket: "bucket", Prefix: "locked", AccessKey: "key", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	// A bucket without Object Lock, and a folder, cannot hold a locked
	// copy, and nothing is uploaded to them unlocked instead.
	dir, r := newRepo(t)
	fill(t, r, 1, "s001")
	folder, _ := NewDir(t.TempDir())
	for _, tg := range []Target{target, folder} {
		if _, err := Sync(ctx, dir, tg, &Lock{Days: 30, Mode: LockCompliance}, nil); err == nil || !strings.Contains(err.Error(), "cannot lock") {
			t.Fatalf("locked sync to %s: %v", tg, err)
		}
		if got, _ := tg.List(ctx); len(got) != 0 {
			t.Fatalf("%d objects were uploaded unlocked to %s", len(got), tg)
		}
	}
	fake.lock = true
	lockedCopy(t, target)

	// Pruning after the locks have run out: what was deleted long
	// enough ago goes, markers included; what is current stays.
	fake.mu.Lock()
	for _, vs := range fake.versions {
		for _, v := range vs {
			v.until = time.Now().Add(-time.Hour)
		}
	}
	fake.mu.Unlock()
	s3 := target.(*s3Target)
	s3.SetLock(LockCompliance, time.Now().Add(time.Hour))
	if err := target.Put(ctx, "packs/aa/current.pack", bytes.NewReader([]byte("live")), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// The two objects that are current: the pack, and the config the
	// attack wrote.
	if len(fake.versions) != 2 || len(fake.versions["locked/packs/aa/current.pack"]) != 1 || len(fake.versions["locked/config.json"]) != 1 {
		t.Fatalf("after pruning, %d objects have versions left", len(fake.versions))
	}
}

func TestLockDate(t *testing.T) {
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	for days, step := range map[int]int{1: 1, 7: 2, 30: 10, 365: 10} {
		d := lockDate(now, days)
		if min, max := now.AddDate(0, 0, days), now.AddDate(0, 0, days+step); d.Before(min) || d.After(max) {
			t.Errorf("%d days: locked until %s, want between %s and %s", days, d, min, max)
		}
		if again := lockDate(now.Add(time.Hour), days); days >= 7 && !again.Equal(d) {
			t.Errorf("%d days: the date moved within an hour, from %s to %s", days, d, again)
		}
	}
}

// TestRealObjectLock runs the locked copy against a real S3-compatible
// service; see TestRealS3 for the settings it needs. The bucket it
// makes cannot be emptied until the locks run out, which is the point.
func TestRealObjectLock(t *testing.T) {
	endpoint := os.Getenv("BB_TEST_S3_ENDPOINT")
	if endpoint == "" || os.Getenv("BB_TEST_S3_LOCK") == "" {
		t.Skip("BB_TEST_S3_ENDPOINT and BB_TEST_S3_LOCK are not both set")
	}
	ctx := context.Background()
	target, err := NewS3(S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: fmt.Sprintf("bb-lock-%d", time.Now().UnixNano()),
		Prefix: "copies/site a", AccessKey: os.Getenv("BB_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("BB_TEST_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	s3 := target.(*s3Target)
	prefix := s3.prefix
	s3.prefix = ""
	resp, err := s3.doWith(ctx, "PUT", "", nil, nil, 0, map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"})
	s3.prefix = prefix
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := s3.CheckLock(ctx); err != nil {
		t.Fatal(err)
	}
	lockedCopy(t, target)
}
