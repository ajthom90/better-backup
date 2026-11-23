package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// pages listings two keys at a time.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	ak, sk  string
	fail    int // fail this many PUTs with a 500 first
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
	switch {
	case r.Method == "GET" && key == "":
		q := r.URL.Query()
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
	case r.Method == "PUT":
		if f.fail > 0 {
			f.fail--
			w.WriteHeader(500)
			return
		}
		f.objects[key] = body
	case r.Method == "GET":
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	case r.Method == "DELETE":
		delete(f.objects, key)
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
			res, err := Sync(ctx, dir, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Packs, one snapshot, and the config.
			if res.Uploaded < 4 || res.Objects != res.Uploaded+1 || res.Deleted != 0 {
				t.Fatalf("first sync: %+v", res)
			}
			// Nothing changed: nothing is sent.
			res2, err := Sync(ctx, dir, target, nil)
			if err != nil || res2.Uploaded != 0 || res2.Objects != res.Objects || res2.Bytes != res.Bytes {
				t.Fatalf("second sync: %+v %v", res2, err)
			}
			// More data and a forgotten snapshot.
			fill(t, r, 2, "bbbb")
			if err := r.DeleteSnapshot("aaaa"); err != nil {
				t.Fatal(err)
			}
			res3, err := Sync(ctx, dir, target, nil)
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
			if _, err := Sync(ctx, other, target, nil); err == nil || !strings.Contains(err.Error(), "different repository") {
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
	res, err := Sync(ctx, dir, target, nil)
	if err != nil || res.Objects < 30 {
		t.Fatalf("%+v %v", res, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "packs")); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, dir, target, nil); err == nil || !strings.Contains(err.Error(), "nothing was removed") {
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
// server. CI starts MinIO for it; elsewhere it is skipped unless
// BB_TEST_S3_ENDPOINT, BB_TEST_S3_ACCESS_KEY and BB_TEST_S3_SECRET_KEY
// are set.
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
	res, err := Sync(ctx, dir, target, nil)
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
	if res, err = Sync(ctx, dir, target, nil); err != nil || res.Uploaded != 0 || res.Deleted != 1 {
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
