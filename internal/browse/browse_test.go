package browse

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ajthom90/better-backup/internal/api"
)

// TestRun lists the machine the test runs on. What it finds differs by
// machine; that it answers, and how folders are shaped, does not.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "b-folder"), 0o755)
	os.WriteFile(filepath.Join(dir, "A-file"), []byte("12345"), 0o644)
	res := Run(context.Background(), &api.BrowseRequest{Kind: api.BrowseFolders, Path: dir})
	if res.Error != "" || len(res.Entries) != 2 || res.Entries[0].Name != "b-folder" || !res.Entries[0].Dir || res.Entries[1].Size != 5 {
		t.Fatalf("listing a folder: %+v", res)
	}
	if res.Up == nil || *res.Up != filepath.Dir(filepath.Clean(dir)) {
		t.Fatalf("the folder above: %v", res.Up)
	}

	top := Run(context.Background(), &api.BrowseRequest{Kind: api.BrowseFolders})
	if top.Error != "" || len(top.Entries) == 0 {
		t.Fatalf("the top: %+v", top)
	}
	// Every entry at the top can be opened in turn.
	first := Run(context.Background(), &api.BrowseRequest{Kind: api.BrowseFolders, Path: top.Entries[0].Value})
	if first.Error == "" && first.Up == nil {
		t.Fatalf("no way up from %s", top.Entries[0].Value)
	}
	if runtime.GOOS == "windows" && (top.Path != "" || len(top.Entries[0].Value) != 3) {
		t.Fatalf("Windows starts at its drives: %+v", top)
	}

	if res := Run(context.Background(), &api.BrowseRequest{Kind: api.BrowseFolders, Path: "relative"}); res.Error == "" {
		t.Fatal("a relative path was listed")
	}
	for _, kind := range []string{api.BrowseVolumes, api.BrowseDisks, api.BrowseVMs, "other"} {
		res := Run(context.Background(), &api.BrowseRequest{Kind: kind})
		if res.Entries == nil {
			t.Fatalf("%s: no list", kind)
		}
		t.Logf("%s: %d entries %+v, error %q", kind, len(res.Entries), res.Entries, res.Error)
	}
	if runtime.GOOS == "windows" {
		if v := Run(context.Background(), &api.BrowseRequest{Kind: api.BrowseVolumes}); v.Error != "" || len(v.Entries) == 0 {
			t.Fatalf("Windows has at least one volume: %+v", v)
		}
		if d := Run(context.Background(), &api.BrowseRequest{Kind: api.BrowseDisks}); d.Error != "" || len(d.Entries) == 0 {
			t.Fatalf("Windows has at least one disk: %+v", d)
		}
	}
}
