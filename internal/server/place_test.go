package server_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// TestPlace moves a new server's repository to another folder and its
// port, from the settings, and checks the server comes back there.
func TestPlace(t *testing.T) {
	defer server.SetInContainer(false)()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	free := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return ln.Addr().(*net.TCPAddr).Port
	}
	first, second := free(), free()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, server.Config{DataDir: data, Listen: "127.0.0.1:" + strconv.Itoa(first), Log: log, Tick: 100 * time.Millisecond})
	}()
	defer func() { cancel(); <-done }()

	connect := func(port int) *adminclient.Client {
		deadline := time.Now().Add(20 * time.Second)
		for {
			var af server.AdminFile
			if b, err := os.ReadFile(filepath.Join(data, "admin.json")); err == nil && jsonUnmarshal(b, &af) == nil && af.Token != "" {
				c, err := adminclient.New("https://127.0.0.1:"+strconv.Itoa(port), af.Token, af.Fingerprint)
				if err == nil && c.Do("GET", "/api/v1/status", nil, nil) == nil {
					return c
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no server on port %d", port)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	admin := connect(first)
	var st api.ServerSettings
	if err := admin.Do("GET", "/api/v1/settings", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Port != first || st.Container || st.RepoDir != filepath.Join(data, "repo") {
		t.Fatalf("settings: %+v", st)
	}

	// Refused: a folder with other files in it, a relative path, a port
	// that is taken.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "holiday.jpg"), []byte("x"), 0o644)
	for _, bad := range []api.PlaceRequest{{RepoDir: other}, {RepoDir: "backups"}, {Port: 70000}} {
		if err := admin.Do("PUT", "/api/v1/settings/place", bad, nil); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}

	dir := filepath.Join(t.TempDir(), "Backups")
	var res struct {
		Restarting bool `json:"restarting"`
		Port       int  `json:"port"`
	}
	if err := admin.Do("PUT", "/api/v1/settings/place", api.PlaceRequest{RepoDir: dir, Port: second}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Restarting || res.Port != second {
		t.Fatalf("answer: %+v", res)
	}
	admin = connect(second)
	if err := admin.Do("GET", "/api/v1/settings", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Port != second || st.RepoDir != dir {
		t.Fatalf("after the move: %+v", st)
	}
	var status api.Status
	if err := admin.Do("GET", "/api/v1/status", nil, &status); err != nil || status.RepoID == "" {
		t.Fatalf("status after the move: %+v %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
		if entries, _ := os.ReadDir(dir); len(entries) == 0 {
			t.Fatal("the repository was not copied to the new folder")
		}
	}

	// The same again changes nothing and does not restart.
	res.Restarting = true
	if err := admin.Do("PUT", "/api/v1/settings/place", api.PlaceRequest{RepoDir: dir, Port: second}, &res); err != nil || res.Restarting {
		t.Fatalf("no change: %+v %v", res, err)
	}

	// In a container the folder and port are not the server's to change.
	defer server.SetInContainer(true)()
	err := admin.Do("PUT", "/api/v1/settings/place", api.PlaceRequest{Port: first}, nil)
	if err == nil || !strings.Contains(err.Error(), "container") {
		t.Fatalf("in a container: %v", err)
	}

	// The log can be read.
	var lg struct {
		Text string `json:"text"`
	}
	if err := admin.Do("POST", "/api/v1/settings/log", nil, &lg); err != nil || !strings.Contains(lg.Text, "starting again") {
		t.Fatalf("log: %q %v", lg.Text, err)
	}
}
