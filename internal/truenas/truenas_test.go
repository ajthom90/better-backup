package truenas

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveConfig(t *testing.T) {
	sys := t.TempDir()
	Database, SecretSeed = filepath.Join(sys, "freenas-v1.db"), filepath.Join(sys, "pwenc_secret")
	AuthorizedKeys, VersionFile = filepath.Join(sys, "authorized_keys"), filepath.Join(sys, "version")
	out := filepath.Join(t.TempDir(), "stage")
	if _, err := SaveConfig(out); err == nil {
		t.Fatal("saved the configuration of a machine that is not TrueNAS")
	}
	os.WriteFile(Database, []byte("settings"), 0o600)
	os.WriteFile(SecretSeed, []byte("seed"), 0o600)
	os.WriteFile(VersionFile, []byte("25.04.2\n"), 0o644)
	sum := func() string {
		t.Helper()
		v, err := SaveConfig(out)
		if err != nil || v != "25.04.2" {
			t.Fatal(v, err)
		}
		f, err := os.Open(filepath.Join(out, ConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var got string
		for tr := tar.NewReader(f); ; {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(tr)
			got += h.Name + "=" + string(b) + ";"
		}
		return got
	}
	if got := sum(); got != "freenas-v1.db=settings;pwenc_secret=seed;" {
		t.Fatal(got)
	}
	first, _ := os.ReadFile(filepath.Join(out, ConfigFile))
	os.WriteFile(AuthorizedKeys, []byte("ssh-ed25519 AAAA"), 0o600)
	if got := sum(); got != "freenas-v1.db=settings;pwenc_secret=seed;root_authorized_keys=ssh-ed25519 AAAA;" {
		t.Fatal(got)
	}
	os.Remove(AuthorizedKeys)
	sum()
	if again, _ := os.ReadFile(filepath.Join(out, ConfigFile)); string(again) != string(first) {
		t.Fatal("an unchanged configuration produced a different file")
	}
}
