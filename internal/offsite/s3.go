package offsite

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// S3Config names a bucket in S3-compatible object storage: Amazon S3,
// Backblaze B2, Wasabi, MinIO, Cloudflare R2 and others.
type S3Config struct {
	// Endpoint is the service URL, such as https://s3.us-west-2.amazonaws.com
	// or https://s3.us-west-004.backblazeb2.com.
	Endpoint string
	// Region is the region the service expects in signatures. Services
	// without regions accept "us-east-1".
	Region string
	Bucket string
	// Prefix is an optional folder inside the bucket.
	Prefix    string
	AccessKey string
	SecretKey string
	// VirtualHost addresses the bucket as bucket.endpoint instead of
	// endpoint/bucket. Few services need it.
	VirtualHost bool
}

type s3Target struct {
	cfg    S3Config
	base   *url.URL
	prefix string
	client *http.Client
	now    func() time.Time

	// lockMode and lockUntil, when set, lock every object Put stores.
	lockMode  string
	lockUntil time.Time
	// asOf, when set, makes this a read-only view of the bucket as it
	// was then; versions are the object versions that view is made of.
	asOf     time.Time
	verMu    sync.Mutex
	versions map[string]string
}

// NewS3 returns a target that keeps the copy in a bucket.
func NewS3(cfg S3Config) (Target, error) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	switch {
	case cfg.Endpoint == "":
		return nil, errors.New("the storage endpoint is not set")
	case cfg.Bucket == "":
		return nil, errors.New("the bucket is not set")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return nil, errors.New("the access key and secret key are not set")
	}
	if !strings.Contains(cfg.Endpoint, "://") {
		cfg.Endpoint = "https://" + cfg.Endpoint
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%q is not a storage endpoint URL", cfg.Endpoint)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.VirtualHost {
		u.Host = cfg.Bucket + "." + u.Host
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	if prefix != "" {
		prefix += "/"
	}
	return &s3Target{cfg: cfg, base: u, prefix: prefix, now: time.Now,
		client: &http.Client{Timeout: 30 * time.Minute}}, nil
}

func (t *s3Target) String() string {
	return "bucket " + t.cfg.Bucket + map[bool]string{true: "/" + strings.TrimSuffix(t.prefix, "/")}[t.prefix != ""] + " at " + t.base.Host
}

// uriEncode encodes as SigV4 requires: everything but unreserved
// characters, and "/" too unless it separates path segments.
func uriEncode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSum(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign returns the Authorization header for a request, following AWS
// Signature Version 4. path is the encoded path; headers must include
// host, x-amz-date and x-amz-content-sha256, with lower-case names.
func sign(method, path string, query url.Values, headers map[string]string, payloadHash, accessKey, secretKey, region string, at time.Time) string {
	var qs []string
	for k, vs := range query {
		for _, v := range vs {
			qs = append(qs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	sort.Strings(qs)
	var names []string
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{method, path, strings.Join(qs, "&"), canon.String(), signed, payloadHash}, "\n")
	day := at.UTC().Format("20060102")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(request))
	toSign := "AWS4-HMAC-SHA256\n" + at.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSum(hmacSum(hmacSum(hmacSum([]byte("AWS4"+secretKey), day), region), "s3"), "aws4_request")
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ",SignedHeaders=" + signed + ",Signature=" + hex.EncodeToString(hmacSum(key, toSign))
}

// do sends one signed request. key is the object key without the
// prefix; an empty key addresses the bucket.
func (t *s3Target) do(ctx context.Context, method, key string, query url.Values, body io.ReadSeeker, size int64) (*http.Response, error) {
	return t.doWith(ctx, method, key, query, body, size, nil)
}

// doWith is do with extra x-amz- headers, which are signed.
func (t *s3Target) doWith(ctx context.Context, method, key string, query url.Values, body io.ReadSeeker, size int64, extra map[string]string) (*http.Response, error) {
	payload, sum5 := emptyHash, ""
	if body != nil {
		h, h5 := sha256.New(), md5.New()
		if _, err := io.Copy(io.MultiWriter(h, h5), body); err != nil {
			return nil, err
		}
		// Requests that lock an object must carry this older checksum.
		sum5 = base64.StdEncoding.EncodeToString(h5.Sum(nil))
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		payload = hex.EncodeToString(h.Sum(nil))
	}
	raw := "/"
	if !t.cfg.VirtualHost {
		raw += t.cfg.Bucket
		if key != "" {
			raw += "/"
		}
	}
	if key != "" {
		raw += t.prefix + key
	}
	if p := strings.TrimRight(t.base.Path, "/"); p != "" {
		raw = p + raw
	}
	u := *t.base
	u.Path, u.RawPath = raw, uriEncode(raw, false)
	u.RawQuery = ""
	if len(query) > 0 {
		var qs []string
		for k, vs := range query {
			for _, v := range vs {
				qs = append(qs, uriEncode(k, true)+"="+uriEncode(v, true))
			}
		}
		sort.Strings(qs)
		u.RawQuery = strings.Join(qs, "&")
	}
	var rd io.Reader
	if body != nil {
		rd = io.NopCloser(body) // keeps net/http from closing the caller's file
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.GetBody = func() (io.ReadCloser, error) {
			_, err := body.Seek(0, io.SeekStart)
			return io.NopCloser(body), err
		}
	}
	at := t.now()
	headers := map[string]string{"host": u.Host, "x-amz-date": at.UTC().Format("20060102T150405Z"), "x-amz-content-sha256": payload}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
		req.Header.Set(k, v)
	}
	if sum5 != "" {
		req.Header.Set("Content-MD5", sum5)
	}
	req.Header.Set("X-Amz-Date", headers["x-amz-date"])
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", sign(method, u.RawPath, query, headers, payload, t.cfg.AccessKey, t.cfg.SecretKey, t.cfg.Region, at))
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNotFound && key != "" {
		return nil, fmt.Errorf("%s: %w", key, fs.ErrNotExist)
	}
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		return nil, fmt.Errorf("the storage service answered %s: %s (%s)", resp.Status, e.Message, e.Code)
	}
	return nil, fmt.Errorf("the storage service answered %s", resp.Status)
}

func (t *s3Target) List(ctx context.Context) ([]Object, error) {
	if !t.asOf.IsZero() {
		return t.listAsOf(ctx)
	}
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}}
		if t.prefix != "" {
			q.Set("prefix", t.prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := t.do(ctx, "GET", "", q, nil, 0)
		if err != nil {
			return nil, err
		}
		var page struct {
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
			Contents    []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading the bucket listing: %w", err)
		}
		for _, c := range page.Contents {
			if name := strings.TrimPrefix(c.Key, t.prefix); name != "" && strings.HasPrefix(c.Key, t.prefix) {
				out = append(out, Object{name, c.Size})
			}
		}
		if !page.IsTruncated || page.Next == "" {
			return out, nil
		}
		token = page.Next
	}
}

func (t *s3Target) Put(ctx context.Context, name string, r io.ReadSeeker, size int64) error {
	if !t.asOf.IsZero() {
		return errReadOnly
	}
	var lock map[string]string
	if t.lockMode != "" {
		lock = map[string]string{"X-Amz-Object-Lock-Mode": t.lockMode, "X-Amz-Object-Lock-Retain-Until-Date": t.lockUntil.UTC().Format(time.RFC3339)}
	}
	// Storage services fail now and then; a pack is small enough to
	// simply send again.
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
			if _, serr := r.Seek(0, io.SeekStart); serr != nil {
				return serr
			}
		}
		var resp *http.Response
		if resp, err = t.doWith(ctx, "PUT", name, nil, r, size, lock); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		if ctx.Err() != nil || strings.Contains(err.Error(), " 40") {
			return err // refused, not a passing fault
		}
	}
	return err
}

func (t *s3Target) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	var q url.Values
	if !t.asOf.IsZero() {
		t.verMu.Lock()
		loaded := t.versions != nil
		t.verMu.Unlock()
		if !loaded {
			if _, err := t.listAsOf(ctx); err != nil {
				return nil, err
			}
		}
		t.verMu.Lock()
		id, ok := t.versions[name]
		t.verMu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
		q = url.Values{"versionId": {id}}
	}
	resp, err := t.do(ctx, "GET", name, q, nil, 0)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (t *s3Target) Delete(ctx context.Context, name string) error {
	if !t.asOf.IsZero() {
		return errReadOnly
	}
	resp, err := t.do(ctx, "DELETE", name, nil, nil, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

var errReadOnly = errors.New("a copy seen as it was at an earlier time cannot be changed")

// ---- Object Lock ----
//
// A bucket created with Object Lock keeps every version of every object,
// and a locked version cannot be deleted or replaced by anyone, with any
// credentials, until its date has passed. Deleting an object only puts a
// marker on top of it; writing it again only adds a version.

const (
	// LockCompliance cannot be shortened or lifted by anyone.
	LockCompliance = "COMPLIANCE"
	// LockGovernance can be lifted by an account with a special
	// permission, which the server never uses.
	LockGovernance = "GOVERNANCE"
)

func (t *s3Target) CheckLock(ctx context.Context) error {
	resp, err := t.do(ctx, "GET", "", url.Values{"object-lock": {""}}, nil, 0)
	if err != nil {
		return fmt.Errorf("%s cannot lock what is stored in it. Object Lock has to be switched on for the bucket, with most services when the bucket is created (%v)", t, err)
	}
	defer resp.Body.Close()
	var c struct {
		Enabled string `xml:"ObjectLockEnabled"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&c); err != nil || c.Enabled != "Enabled" {
		return fmt.Errorf("%s cannot lock what is stored in it. Object Lock has to be switched on for the bucket, with most services when the bucket is created", t)
	}
	return nil
}

func (t *s3Target) SetLock(mode string, until time.Time) { t.lockMode, t.lockUntil = mode, until }

func (t *s3Target) Relock(ctx context.Context, name string, until time.Time) error {
	body := fmt.Sprintf(`<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>%s</Mode><RetainUntilDate>%s</RetainUntilDate></Retention>`, t.lockMode, until.UTC().Format(time.RFC3339))
	resp, err := t.do(ctx, "PUT", name, url.Values{"retention": {""}}, bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		// A lock cannot be made shorter. If the object is already
		// locked for at least as long, all is well.
		if head, herr := t.do(ctx, "HEAD", name, nil, nil, 0); herr == nil {
			head.Body.Close()
			if has, perr := time.Parse(time.RFC3339, head.Header.Get("X-Amz-Object-Lock-Retain-Until-Date")); perr == nil && !has.Before(until) {
				return nil
			}
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// version is one version of an object, or a delete marker.
type version struct {
	XMLName  xml.Name
	Key      string    `xml:"Key"`
	ID       string    `xml:"VersionId"`
	Latest   bool      `xml:"IsLatest"`
	Modified time.Time `xml:"LastModified"`
	Size     int64     `xml:"Size"`
}

func (v *version) marker() bool { return v.XMLName.Local == "DeleteMarker" }

// listVersions returns every version of every object under the prefix:
// objects in name order, and for each object the newest version first.
func (t *s3Target) listVersions(ctx context.Context) ([]version, error) {
	var out []version
	keyMarker, idMarker := "", ""
	for {
		q := url.Values{"versions": {""}}
		if t.prefix != "" {
			q.Set("prefix", t.prefix)
		}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
			if idMarker != "" {
				q.Set("version-id-marker", idMarker)
			}
		}
		resp, err := t.do(ctx, "GET", "", q, nil, 0)
		if err != nil {
			return nil, err
		}
		var page struct {
			IsTruncated bool      `xml:"IsTruncated"`
			NextKey     string    `xml:"NextKeyMarker"`
			NextID      string    `xml:"NextVersionIdMarker"`
			Items       []version `xml:",any"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading the bucket's list of versions: %w", err)
		}
		for _, v := range page.Items {
			if (v.XMLName.Local == "Version" || v.marker()) && strings.HasPrefix(v.Key, t.prefix) && v.Key != t.prefix {
				v.Key = strings.TrimPrefix(v.Key, t.prefix)
				out = append(out, v)
			}
		}
		if !page.IsTruncated || page.NextKey == "" {
			break
		}
		keyMarker, idMarker = page.NextKey, page.NextID
	}
	// The service sends this order already; a stable sort only makes
	// sure of it without disturbing versions written in the same moment.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Modified.After(out[j].Modified)
	})
	return out, nil
}

// AsOf returns the copy as it was at a moment in the past: for every
// object the version that was current then, whatever was deleted or
// written over it since.
func (t *s3Target) AsOf(at time.Time) Target {
	return &s3Target{cfg: t.cfg, base: t.base, prefix: t.prefix, client: t.client, now: t.now, asOf: at}
}

func (t *s3Target) listAsOf(ctx context.Context) ([]Object, error) {
	all, err := t.listVersions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Object
	ids := map[string]string{}
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].Key == all[i].Key {
			j++
		}
		for _, v := range all[i:j] {
			if v.Modified.After(t.asOf) {
				continue
			}
			if !v.marker() {
				out = append(out, Object{v.Key, v.Size})
				ids[v.Key] = v.ID
			}
			break
		}
		i = j
	}
	t.verMu.Lock()
	t.versions = ids
	t.verMu.Unlock()
	return out, nil
}

// Prune removes the versions that stopped being current before cutoff:
// what was deleted or written over that long ago. Versions that are
// still locked are refused by the service and stay for a later run.
func (t *s3Target) Prune(ctx context.Context, cutoff time.Time) (int, error) {
	all, err := t.listVersions(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	remove := func(v version) bool {
		resp, err := t.do(ctx, "DELETE", v.Key, url.Values{"versionId": {v.ID}}, nil, 0)
		if err != nil {
			return errors.Is(err, fs.ErrNotExist)
		}
		resp.Body.Close()
		n++
		return true
	}
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].Key == all[i].Key {
			j++
		}
		vs := all[i:j]
		i = j
		gone := true
		// From the oldest up: each stopped being current when the
		// version above it was written.
		for k := len(vs) - 1; k >= 1; k-- {
			if !vs[k-1].Modified.Before(cutoff) || !remove(vs[k]) {
				gone = false
			}
			if err := ctx.Err(); err != nil {
				return n, err
			}
		}
		// A marker with nothing left under it marks nothing.
		if gone && vs[0].marker() && vs[0].Modified.Before(cutoff) {
			remove(vs[0])
		}
	}
	return n, nil
}
