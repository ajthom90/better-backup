package server_test

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
)

// The server hands out the agent installers it holds, and nothing else
// from that folder.
func TestInstallers(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "installers")
	os.MkdirAll(dir, 0o700)
	const name = "better-backup-agent_1.2.3_windows_amd64.msi"
	write(t, filepath.Join(dir, name), []byte("an installer"))
	write(t, filepath.Join(dir, "notes.txt"), []byte("not an installer"))

	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { err := srv.Serve(ctx); srv.Close(); done <- err }()
	defer func() { cancel(); <-done }()
	base := "https://" + srv.Addr()

	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	var list api.Installers
	if err := admin.Do("GET", "/api/v1/installers", nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 1 || list.Files[0].Name != name || list.Files[0].OS != "windows" || list.Files[0].Arch != "amd64" ||
		list.Files[0].Format != "msi" || list.Files[0].Version != "1.2.3" || list.Files[0].URL != "/installers/"+name {
		t.Fatalf("unexpected list: %+v", list.Files)
	}

	// The download needs no sign-in: it is fetched from the new machine.
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	get := func(path string) (int, string) {
		resp, err := hc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(list.Files[0].URL); code != 200 || body != "an installer" {
		t.Fatalf("download: %d %q", code, body)
	}
	for _, p := range []string{"/installers/notes.txt", "/installers/..%2Fadmin.json", "/installers/missing_1_linux_amd64.deb"} {
		if code, _ := get(p); code != 404 {
			t.Fatalf("%s: got %d, want 404", p, code)
		}
	}
}
