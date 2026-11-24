// Package truenas saves the configuration of a TrueNAS system: what its
// own "Download File" button under Manage Configuration produces.
package truenas

import (
	"archive/tar"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Where TrueNAS keeps its configuration. Tests replace these.
var (
	Database       = "/data/freenas-v1.db"
	SecretSeed     = "/data/pwenc_secret"
	AuthorizedKeys = "/root/.ssh/authorized_keys"
	VersionFile    = "/etc/version"
)

// ConfigFile is the name of the saved configuration. TrueNAS accepts it
// as it is under System > General Settings > Manage Configuration >
// Upload File.
const ConfigFile = "truenas-config.tar"

// Version returns the TrueNAS version of this machine, and false when
// it is not a TrueNAS system.
func Version() (string, bool) {
	if _, err := os.Stat(Database); err != nil {
		return "", false
	}
	b, _ := os.ReadFile(VersionFile)
	return strings.TrimSpace(string(b)), true
}

// SaveConfig writes the configuration into dir, which it creates: the
// database with every setting, the secret seed that decrypts the
// passwords stored in it, and root's authorized SSH keys, in one tar
// file laid out the way TrueNAS makes it, and beside it a note with the
// version it came from. It returns that version.
//
// The files are read through the system's own paths rather than its
// API: they have not moved since FreeNAS 11, and the API's own export
// packs the same files.
func SaveConfig(dir string) (version string, err error) {
	version, ok := Version()
	if !ok {
		return "", errors.New("this machine is not a TrueNAS system: it has no " + Database)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out, err := os.OpenFile(filepath.Join(dir, ConfigFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	tw := tar.NewWriter(out)
	var newest time.Time
	for _, m := range []struct {
		name, path string
		optional   bool
	}{{"freenas-v1.db", Database, false}, {"pwenc_secret", SecretSeed, false}, {"root_authorized_keys", AuthorizedKeys, true}} {
		data, rerr := os.ReadFile(m.path)
		fi, serr := os.Stat(m.path)
		if rerr != nil || serr != nil {
			if m.optional {
				continue
			}
			out.Close()
			return "", fmt.Errorf("reading %s: %w", m.path, errors.Join(rerr, serr))
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		// Only what cannot differ between two saves of the same
		// settings goes into the headers, so an unchanged configuration
		// is an unchanged file.
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o600, Size: int64(len(data)), ModTime: fi.ModTime().Truncate(time.Second), Format: tar.FormatPAX}); err != nil {
			out.Close()
			return "", err
		}
		if _, err := tw.Write(data); err != nil {
			out.Close()
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	note := "TrueNAS version: " + version + "\n\n" +
		"To put this configuration back: in the TrueNAS web interface open System, General Settings,\n" +
		"Manage Configuration, Upload File and choose " + ConfigFile + ". The system restarts with these\n" +
		"settings. Upload it to the same TrueNAS version or a newer one.\n"
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte(note), 0o600); err != nil {
		return "", err
	}
	for _, name := range []string{ConfigFile, "README.txt"} {
		os.Chtimes(filepath.Join(dir, name), newest, newest)
	}
	return version, nil
}
