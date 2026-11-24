package m365

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/m365/m365test"
)

func TestClient(t *testing.T) {
	retryBase = time.Millisecond
	ms := m365test.New()
	defer ms.Close()
	ctx := context.Background()
	c := New(Connection{TenantID: ms.TenantID, ClientID: ms.ClientID, ClientSecret: ms.Secret, LoginURL: ms.URL, GraphURL: ms.URL + "/"})

	// Throttled requests are tried again; pages are followed.
	ms.Lock()
	ms.Throttle = 3
	ms.Unlock()
	us, err := c.Users(ctx)
	if err != nil || len(us) != 4 || us[3].Guest != true || us[2].Licensed {
		t.Fatalf("users: %+v %v", us, err)
	}
	ms.Lock()
	if ms.Requests != 5 {
		t.Fatalf("%d requests for two pages after three refusals", ms.Requests)
	}
	// Throttling that does not end is an error, not a loop.
	ms.Throttle = 1000
	ms.Unlock()
	if _, err := c.Users(ctx); err == nil || !strings.Contains(err.Error(), "TooManyRequests") {
		t.Fatalf("endless throttling: %v", err)
	}
	ms.Lock()
	ms.Throttle = 0
	ms.Unlock()

	// A guest's sign-in name holds a #, which must survive the address.
	u, err := c.User(ctx, "carol_partner.com#EXT#@example.com")
	if err != nil || u.ID != "u-guest" {
		t.Fatalf("guest: %+v %v", u, err)
	}
	if _, err := c.User(ctx, "nobody@example.com"); !NotFound(err) {
		t.Fatalf("missing user: %v", err)
	}

	// The token is not sent along a link that leaves Graph.
	var got string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer other.Close()
	if _, err := c.get(ctx, other.URL+"/v1.0/users"); err == nil || got != "" {
		t.Fatalf("a link to another server: %v, token sent: %q", err, got)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{"Re: a/b": "Re_ a_b", "  trailing. . ": "trailing", "": "_", "..": "_", "tab\there": "tab_here", "ünï": "ünï"} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
