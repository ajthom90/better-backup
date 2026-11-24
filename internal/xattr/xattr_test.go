//go:build linux || darwin

package xattr

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadWrite(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	os.WriteFile(src, nil, 0o644)
	os.WriteFile(dst, nil, 0o644)
	if attrs, _, err := Read(src); err != nil || len(attrs) != 0 {
		t.Fatal(attrs, err)
	}
	big := bytes.Repeat([]byte{7}, 3000)
	if err := unix.Setxattr(src, "user.big", big, 0); err != nil {
		t.Skip("this file system has no extended attributes:", err)
	}
	unix.Setxattr(src, "user.empty", nil, 0)
	attrs, skipped, err := Read(src)
	if err != nil || len(skipped) != 0 || len(attrs) != 2 || !bytes.Equal(attrs["user.big"], big) || len(attrs["user.empty"]) != 0 {
		t.Fatal(len(attrs), skipped, err)
	}
	// Of the link itself, not of its target.
	link := filepath.Join(dir, "link")
	os.Symlink(src, link)
	if attrs, _, err := Read(link); err != nil || len(attrs) != 0 {
		t.Fatal(attrs, err)
	}
	if failed, err := Write(dst, attrs); err != nil {
		t.Fatal(failed, err)
	}
	if back, _, _ := Read(dst); len(back) != 2 || !bytes.Equal(back["user.big"], big) {
		t.Fatal("attributes differ after writing them back")
	}
	// Linux refuses a name outside its namespaces; macOS takes any name.
	if runtime.GOOS == "linux" {
		if failed, err := Write(dst, map[string][]byte{"user.ok": {1}, "nonsense": {1}}); err == nil || len(failed) != 1 || failed[0] != "nonsense" {
			t.Fatal(failed, err)
		}
	}
}
