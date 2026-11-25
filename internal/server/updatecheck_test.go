package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

func setVersion(t *testing.T, v string) {
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

func TestUpdateCheck(t *testing.T) {
	setVersion(t, "1.2.0")
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	type rel struct {
		Tag        string  `json:"tag_name"`
		Name       string  `json:"name"`
		Body       string  `json:"body"`
		URL        string  `json:"html_url"`
		Draft      bool    `json:"draft"`
		Prerelease bool    `json:"prerelease"`
		Published  string  `json:"published_at,omitempty"`
		Assets     []asset `json:"assets"`
	}
	var list atomic.Value
	var asked atomic.Int32
	var status atomic.Int32
	status.Store(200)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		// Nothing about this server leaves it.
		if r.URL.RawQuery != "" || r.Header.Get("User-Agent") != "better-backup" || r.ContentLength > 0 || strings.Contains(r.Header.Get("User-Agent"), Version) {
			t.Errorf("the request says more than it should: %s %v", r.URL, r.Header)
		}
		if c := int(status.Load()); c != 200 {
			http.Error(w, "no", c)
			return
		}
		json.NewEncoder(w).Encode(list.Load())
	}))
	defer gh.Close()
	list.Store([]rel{{Tag: "v1.2.0", Body: "the running one", Published: "2026-11-01T00:00:00Z"}, {Tag: "v1.1.0", Body: "old"}})

	s, err := New(Config{DataDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), UpdateURL: gh.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mail := func() string {
		t.Helper()
		select {
		case m := <-s.mail:
			return m.subject + "\n" + m.body
		default:
			return ""
		}
	}

	// A release build follows the releases, and is up to date.
	if u := s.serverUpdate(); u.Channel != api.ChannelStable || !u.Check || !u.Alert || !u.CanCheck || u.Latest != "" || u.Checked != nil || !u.Numbered || !u.CurrentStable {
		t.Fatalf("before the first check: %+v", u)
	}
	s.checkUpdate(context.Background())
	if u := s.serverUpdate(); u.Latest != "" || len(u.Releases) != 0 || u.Checked == nil || u.Error != "" {
		t.Fatalf("up to date: %+v", u)
	}
	if m := mail(); m != "" {
		t.Fatalf("an alert without a newer version:\n%s", m)
	}

	// Two newer releases, a release candidate, a draft and a build from
	// main: the stable channel sees the releases only.
	sys := map[string]string{"linux": "linux", "darwin": "darwin", "windows": "windows"}[runtime.GOOS]
	list.Store([]rel{
		{Tag: "edge", Name: "1.4.1-edge.3", Prerelease: true, Body: "### Fixed\n\n- on main", URL: "https://example.com/edge"},
		{Tag: "v1.5.0-rc.1", Prerelease: true, Body: "candidate"},
		{Tag: "v9.0.0", Draft: true, Body: "not out"},
		{Tag: "v1.4.0", Body: "### Added\r\n\r\n- Graphs", URL: "https://example.com/v1.4.0", Published: "2026-12-02T10:00:00Z", Assets: []asset{
			{Name: "better-backup-server_1.4.0_" + sys + "_" + runtime.GOARCH + ".msi", URL: "https://example.com/dl/server.msi"},
			{Name: "better-backup-server_1.4.0_" + sys + "_" + runtime.GOARCH + ".deb", URL: "https://example.com/dl/server.deb"},
			{Name: "better-backup-server_1.4.0_darwin_universal.pkg", URL: "https://example.com/dl/server.pkg"},
			{Name: "better-backup-server_1.4.0_" + sys + "_" + runtime.GOARCH + ".rpm", URL: "javascript:alert(1)"},
			{Name: "better-backup-agent_1.4.0_" + sys + "_" + runtime.GOARCH + ".deb", URL: "https://example.com/dl/agent.deb"}}},
		{Tag: "v1.3.0", Body: "### Fixed\n\n- A bug", URL: "javascript:alert(1)"},
		{Tag: "v1.2.0", Body: "the running one"}, {Tag: "nightly", Body: "not a version"}})
	s.checkUpdate(context.Background())
	u := s.serverUpdate()
	if u.Latest != "1.4.0" || len(u.Releases) != 2 || u.Releases[0].Version != "1.4.0" || u.Releases[1].Version != "1.3.0" ||
		u.Releases[0].Notes != "### Added\n\n- Graphs" || u.Releases[0].URL != "https://example.com/v1.4.0" || u.Releases[1].URL != "" {
		t.Fatalf("two newer releases: %+v", u)
	}
	if d := u.Releases[0].Downloads; len(d) != 1 || !strings.HasPrefix(d[0].Name, "better-backup-server_1.4.0_") || !strings.HasPrefix(d[0].URL, "https://example.com/dl/server.") {
		t.Errorf("the downloads for this system: %+v", d)
	}
	m := mail()
	for _, want := range []string{"Version 1.4.0 of better-backup is out", "runs version 1.2.0", "How to update: ", "What is new in 1.4.0", "- Graphs", "What is new in 1.3.0", "- A bug", "https://example.com/v1.4.0"} {
		if !strings.Contains(m, want) {
			t.Errorf("the alert lacks %q:\n%s", want, m)
		}
	}
	// Once per version.
	s.checkUpdate(context.Background())
	if m := mail(); m != "" {
		t.Fatalf("a second alert about the same version:\n%s", m)
	}

	// GitHub is out of reach: what was known stays, with the reason.
	status.Store(403)
	s.checkUpdate(context.Background())
	if u := s.serverUpdate(); u.Latest != "1.4.0" || !strings.Contains(u.Error, "403") {
		t.Fatalf("after a failed check: %+v", u)
	}
	status.Store(200)

	// The edge channel also hears of builds from main and of release
	// candidates. Alerts are off: none goes out.
	save := func(u api.ServerUpdate) {
		b, _ := json.Marshal(updateSettings{Off: !u.Check, Channel: u.Channel, NoAlert: !u.Alert})
		s.setSetting(updateSettingsKey, string(b))
	}
	save(api.ServerUpdate{Channel: api.ChannelEdge, Check: true})
	if u := s.serverUpdate(); u.Latest != "" || u.Checked != nil {
		t.Fatalf("the other channel's answer was shown: %+v", u)
	}
	s.checkUpdate(context.Background())
	u = s.serverUpdate()
	if u.Latest != "1.5.0-rc.1" || len(u.Releases) != 4 || u.Releases[1].Version != "1.4.1-edge.3" || !u.Releases[1].Prerelease || u.Releases[2].Version != "1.4.0" {
		t.Fatalf("the edge channel: %+v", u)
	}
	if m := mail(); m != "" {
		t.Fatalf("an alert though alerts are off:\n%s", m)
	}

	// After the update the same answer says: up to date.
	setVersion(t, "1.5.0-rc.1")
	if u := s.serverUpdate(); u.Latest != "" || len(u.Releases) != 0 || u.CurrentStable {
		t.Fatalf("after updating: %+v", u)
	}

	// Turned off: no question is asked, and nothing is shown.
	save(api.ServerUpdate{Channel: api.ChannelEdge})
	n := asked.Load()
	s.updateNext.Store(0)
	s.scheduleUpdateCheck(time.Now())
	time.Sleep(100 * time.Millisecond)
	if asked.Load() != n {
		t.Error("asked though the check is off")
	}
	if u := s.serverUpdate(); u.Check || u.Latest != "" {
		t.Fatalf("turned off: %+v", u)
	}

	// Turned on again: asked in the background, and not again until the
	// answer is old.
	setVersion(t, "1.2.0")
	save(api.ServerUpdate{Channel: api.ChannelStable, Check: true, Alert: true})
	s.updateNext.Store(0)
	s.scheduleUpdateCheck(time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for asked.Load() == n || s.updateBusy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the check did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n = asked.Load()
	s.updateNext.Store(0)
	s.scheduleUpdateCheck(time.Now())
	s.scheduleUpdateCheck(time.Now().Add(updateEvery - time.Minute))
	time.Sleep(100 * time.Millisecond)
	if asked.Load() != n {
		t.Error("asked again though the answer is fresh")
	}
	if s.serverUpdate().Latest != "1.4.0" {
		t.Errorf("after the background check: %+v", s.serverUpdate())
	}
}

func TestUpdateChannelOfBuild(t *testing.T) {
	for v, want := range map[string]string{"1.2.0": api.ChannelStable, "1.2.1-edge.4": api.ChannelEdge, "1.3.0-rc.1": api.ChannelEdge,
		"edge-0d5dd8f1c2aa": api.ChannelEdge, "dev": api.ChannelStable} {
		setVersion(t, v)
		if got := buildChannel(); got != want {
			t.Errorf("channel of %s = %s, want %s", v, got, want)
		}
	}
	// The images from before versions were numbered are older than
	// anything numbered; a build from source is never told to update.
	setVersion(t, "edge-0d5dd8f1c2aa")
	if !isNewer("0.1.0-edge.170") {
		t.Error("an unnumbered edge image should be older than a numbered one")
	}
	setVersion(t, "dev")
	if isNewer("9.0.0") {
		t.Error("a build from source was told to update")
	}
}
