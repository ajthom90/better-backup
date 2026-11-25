package server_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// Ntfy gets every alert with the priority and tags of its level, signs
// in the way it was told to, and never hands its secrets back.
func TestNtfy(t *testing.T) {
	type got struct {
		auth string
		msg  struct {
			Topic, Title, Message string
			Priority              int
			Tags                  []string
		}
	}
	var mu sync.Mutex
	var gots []got
	status := 200
	ntfy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var g got
		g.auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&g.msg)
		mu.Lock()
		gots = append(gots, g)
		st := status
		mu.Unlock()
		if st != 200 {
			rw.WriteHeader(st)
			rw.Write([]byte(`{"code":40101,"http":401,"error":"unauthorized"}`))
		}
	}))
	defer ntfy.Close()
	last := func(title string) *got {
		mu.Lock()
		defer mu.Unlock()
		for i := len(gots) - 1; i >= 0; i-- {
			if strings.Contains(gots[i].msg.Title, title) {
				return &gots[i]
			}
		}
		return nil
	}

	data := t.TempDir()
	srv, _ := startServer(t, server.Config{DataDir: data, Listen: "127.0.0.1:0", Tick: 50 * time.Millisecond, Log: quiet})
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	w := newWeb(t, srv.Addr()).as(af.Token)

	put := func(n api.NtfySettings) api.AlertSettings {
		t.Helper()
		var out api.AlertSettings
		w.must("PUT", "/api/v1/alerts", api.AlertSettings{Ntfy: &n}, &out)
		return out
	}
	for _, bad := range []api.NtfySettings{
		{Enabled: true},
		{Topic: "no spaces"},
		{Topic: "t", Server: "ftp://ntfy.example.com"},
		{Topic: "t", Token: "tk_x", Username: "u", Password: "p"},
		{Topic: "t", Levels: map[string]api.NtfyLevel{"failed": {Priority: 6}}},
		{Topic: "t", Levels: map[string]api.NtfyLevel{"loud": {Priority: 5}}},
	} {
		w.refused(400, "PUT", "/api/v1/alerts", api.AlertSettings{Ntfy: &bad})
	}
	w.refused(400, "POST", "/api/v1/alerts/test-ntfy", nil)

	// A token is kept, used, and not shown again; levels get defaults.
	out := put(api.NtfySettings{Enabled: true, Server: ntfy.URL + "/", Topic: "backups", Token: "tk_secret"})
	if n := out.Ntfy; n == nil || n.Token != "" || !n.TokenSet || n.Server != ntfy.URL || n.Levels["security"].Priority != 5 || n.Levels["info"].Tags == "" {
		t.Fatalf("saved ntfy settings: %+v", out.Ntfy)
	}
	w.must("POST", "/api/v1/alerts/test-ntfy", nil, nil)
	g := last("Test message")
	if g == nil || g.auth != "Bearer tk_secret" || g.msg.Topic != "backups" || g.msg.Priority != 4 || !slices.Equal(g.msg.Tags, []string{"x"}) || strings.Contains(g.msg.Title, "[better-backup]") {
		t.Fatalf("test notification: %+v", g)
	}

	// Saving again without the token keeps it; a user name and password
	// replace it. Levels are the user's own.
	out.Ntfy.Levels["failed"] = api.NtfyLevel{Priority: 5, Tags: " skull , backup "}
	out.Ntfy.Levels["warning"] = api.NtfyLevel{Priority: 1}
	put(*out.Ntfy)
	w.must("POST", "/api/v1/alerts/test-ntfy", nil, nil)
	if g := last("Test message"); g.auth != "Bearer tk_secret" || g.msg.Priority != 5 || !slices.Equal(g.msg.Tags, []string{"skull", "backup"}) {
		t.Fatalf("after saving again: %+v", g)
	}
	w.must("POST", "/api/v1/alerts/test-ntfy?level=warning", nil, nil)
	if g := last("Test message"); g.msg.Priority != 1 || len(g.msg.Tags) != 0 {
		t.Fatalf("a level without tags: %+v", g)
	}
	out = put(api.NtfySettings{Enabled: true, Server: ntfy.URL, Topic: "backups", Username: "andrew", Password: "pässword"})
	if out.Ntfy.Password != "" || !out.Ntfy.PasswordSet || out.Ntfy.TokenSet {
		t.Fatalf("saved a password: %+v", out.Ntfy)
	}
	w.must("POST", "/api/v1/alerts/test-ntfy", nil, nil)
	if g := last("Test message"); g.auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("andrew:pässword")) {
		t.Fatalf("basic sign-in: %q", g.auth)
	}

	// Ntfy alone counts as alerts being on.
	var h api.Health
	w.must("GET", "/api/v1/health", nil, &h)
	for _, i := range h.Issues {
		if strings.Contains(i.Text, "nobody is told") {
			t.Fatalf("health with ntfy on: %s", i.Text)
		}
	}

	// A refused sign-in is said in plain words.
	mu.Lock()
	status = 401
	mu.Unlock()
	if msg := w.refused(502, "POST", "/api/v1/alerts/test-ntfy", nil); !strings.Contains(msg, "did not accept the sign-in") {
		t.Fatalf("refused sign-in: %s", msg)
	}
	mu.Lock()
	status = 200
	mu.Unlock()

	// Switching ntfy off tells the topic first, as a security alert.
	put(api.NtfySettings{Server: ntfy.URL, Topic: "backups", Username: "andrew", PasswordSet: true})
	waitFor(t, "ntfy to hear that alerts were switched off", func() bool {
		g := last("alert settings were changed")
		return g != nil && g.msg.Priority == 5 && slices.Equal(g.msg.Tags, []string{"rotating_light"})
	})
}
