package offsite

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
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
	payload := emptyHash
	if body != nil {
		h := sha256.New()
		if _, err := io.Copy(h, body); err != nil {
			return nil, err
		}
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
		if resp, err = t.do(ctx, "PUT", name, nil, r, size); err == nil {
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
	resp, err := t.do(ctx, "GET", name, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (t *s3Target) Delete(ctx context.Context, name string) error {
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
