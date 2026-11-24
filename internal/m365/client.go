// Package m365 reads mailboxes and OneDrive from Microsoft 365 through
// Microsoft Graph, as an application that an administrator of the
// tenant has registered and consented to.
package m365

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultLogin = "https://login.microsoftonline.com"
	defaultGraph = "https://graph.microsoft.com"
)

// Connection is how to sign in to a tenant.
type Connection struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// LoginURL and GraphURL are for the national clouds; empty means
	// the worldwide service.
	LoginURL string
	GraphURL string
}

// Client talks to Microsoft Graph for one tenant.
type Client struct {
	conn  Connection
	login string
	graph string
	http  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// retryBase is the shortest wait before a request is tried again.
var retryBase = 2 * time.Second

func New(conn Connection) *Client {
	c := &Client{conn: conn, login: strings.TrimRight(conn.LoginURL, "/"), graph: strings.TrimRight(conn.GraphURL, "/")}
	if c.login == "" {
		c.login = defaultLogin
	}
	if c.graph == "" {
		c.graph = defaultGraph
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 2 * time.Minute
	c.http = &http.Client{Transport: tr}
	return c
}

// Error is a refusal from Microsoft.
type Error struct {
	Status  int
	Code    string
	Message string
	// Hint says what usually causes it.
	Hint string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" && !strings.Contains(msg, e.Code) {
		msg = e.Code + ": " + msg
	}
	if e.Hint != "" {
		msg += " (" + e.Hint + ")"
	}
	return "Microsoft 365: " + msg
}

// NotFound reports whether err says the thing asked for does not
// exist: a user without a mailbox, or without a OneDrive.
func NotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusNotFound || e.Code == "MailboxNotEnabledForRESTAPI" || e.Code == "MailboxNotSupportedForRESTAPI"
}

func denied(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized)
}

// hint adds, to a refusal for lack of rights, the permission that is
// missing.
func hint(err error, permission string) error {
	var e *Error
	if errors.As(err, &e) && denied(err) && e.Hint == "" {
		e.Hint = "the app registration needs the application permission " + permission + ", with admin consent granted"
	}
	return err
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	if c.conn.TenantID == "" || c.conn.ClientID == "" || c.conn.ClientSecret == "" {
		return "", errors.New("Microsoft 365: the tenant id, the application id and the client secret are all needed")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.conn.ClientID}, "client_secret": {c.conn.ClientSecret},
		"scope": {c.graph + "/.default"}}
	req, err := http.NewRequestWithContext(ctx, "POST", c.login+"/"+url.PathEscape(c.conn.TenantID)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("Microsoft 365: signing in: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.Unmarshal(body, &t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		e := &Error{Status: resp.StatusCode, Code: t.Error, Message: "sign-in was refused"}
		// The first line of the description says what is wrong; the
		// rest is trace ids.
		if d, _, _ := strings.Cut(t.Description, "\n"); strings.TrimSpace(d) != "" {
			e.Message = strings.TrimSpace(d)
		}
		switch {
		case strings.Contains(t.Description, "AADSTS7000215"), strings.Contains(t.Description, "AADSTS7000222"):
			e.Hint = "the client secret is wrong or has expired; create a new one under Certificates & secrets and paste its Value, not its Secret ID"
		case strings.Contains(t.Description, "AADSTS700016"):
			e.Hint = "the application (client) id is not one of this tenant"
		case strings.Contains(t.Description, "AADSTS90002"), strings.Contains(t.Description, "AADSTS900023"):
			e.Hint = "the directory (tenant) id is wrong"
		}
		return "", e
	}
	if t.ExpiresIn < 120 {
		t.ExpiresIn = 120
	}
	c.token, c.expires = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn-60)*time.Second)
	return c.token, nil
}

// url makes a Graph address of a path below /v1.0. A whole address, as
// in a link to the next page, is accepted only if it is Graph's own:
// the token goes nowhere else.
func (c *Client) url(path string) (string, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if !strings.HasPrefix(path, c.graph+"/") {
			return "", fmt.Errorf("Microsoft 365: a link leads away from %s", c.graph)
		}
		return path, nil
	}
	return c.graph + "/v1.0" + path, nil
}

// get asks Graph for path and returns the answer, which the caller
// closes. Throttling and passing failures are waited out.
func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	u, err := c.url(path)
	if err != nil {
		return nil, err
	}
	var last error
	for try := 0; try < 7; try++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		wait := retryBase << try
		resp, err := c.http.Do(req)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = fmt.Errorf("Microsoft 365: %w", err)
		case resp.StatusCode < 300:
			return resp, nil
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var g struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(body, &g)
			e := &Error{Status: resp.StatusCode, Code: g.Error.Code, Message: g.Error.Message}
			last = e
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				// The token may have been revoked early: sign in again, once.
				c.mu.Lock()
				c.token = ""
				c.mu.Unlock()
				if try > 0 {
					return nil, e
				}
				wait = 0
			case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
					wait = max(time.Duration(s)*time.Second, retryBase)
				}
			default:
				return nil, e
			}
		}
		select {
		case <-time.After(min(wait, 5*time.Minute)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("Microsoft 365: reading the answer: %w", err)
	}
	return nil
}

// list reads every page of a collection into out, a pointer to a slice.
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	for path != "" {
		var page struct {
			Value []T    `json:"value"`
			Next  string `json:"@odata.nextLink"`
		}
		if err := c.getJSON(ctx, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		path = page.Next
	}
	return all, nil
}

// User is one account of the tenant.
type User struct {
	ID   string `json:"id"`
	UPN  string `json:"upn"`
	Name string `json:"name"`
	// Licensed says the account has a Microsoft 365 licence, which is
	// what gives it a mailbox or a OneDrive of its own.
	Licensed bool `json:"licensed"`
	Guest    bool `json:"guest,omitempty"`
}

type graphUser struct {
	ID       string            `json:"id"`
	UPN      string            `json:"userPrincipalName"`
	Name     string            `json:"displayName"`
	Type     string            `json:"userType"`
	Licenses []json.RawMessage `json:"assignedLicenses"`
}

func (g graphUser) user() User {
	return User{ID: g.ID, UPN: g.UPN, Name: g.Name, Licensed: len(g.Licenses) > 0, Guest: strings.EqualFold(g.Type, "Guest")}
}

const userFields = "$select=id,userPrincipalName,displayName,userType,assignedLicenses"

// Users lists the accounts of the tenant.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	gs, err := list[graphUser](ctx, c, "/users?"+userFields+"&$top=999")
	if err != nil {
		return nil, hint(err, "User.Read.All")
	}
	out := make([]User, len(gs))
	for i, g := range gs {
		out[i] = g.user()
	}
	return out, nil
}

// User finds one account by its sign-in name.
func (c *Client) User(ctx context.Context, upn string) (*User, error) {
	var g graphUser
	if err := c.getJSON(ctx, "/users/"+url.PathEscape(upn)+"?"+userFields, &g); err != nil {
		return nil, hint(err, "User.Read.All")
	}
	u := g.user()
	return &u, nil
}
