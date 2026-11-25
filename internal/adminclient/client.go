// Package adminclient is a small client for the server's admin API.
package adminclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// Client calls the admin API with a bearer token over pinned TLS.
type Client struct {
	base, token string
	http        *http.Client
}

// New connects to a server, pinning its CA by fingerprint.
func New(serverURL, token, fingerprint string) (*Client, error) {
	serverURL = strings.TrimRight(serverURL, "/")
	caPEM, err := api.FetchCA(serverURL, fingerprint)
	if err != nil {
		return nil, err
	}
	cfg, err := api.PinnedTLS(caPEM, nil)
	if err != nil {
		return nil, err
	}
	return &Client{base: serverURL, token: token, http: &http.Client{Timeout: 2 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: cfg}}}, nil
}

// Do performs a request, decoding the JSON response into out if set.
func (c *Client) Do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e api.Error
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Raw performs a request and returns the body as it came, for answers
// that are not JSON.
func (c *Client) Raw(method, path string) ([]byte, error) {
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	return data, nil
}

// Base is the address of the server, as given to New.
func (c *Client) Base() string { return c.base }
