package agent

import (
	"errors"
	"os"
)

// fullDiskAccess reports whether macOS lets this process read files it
// protects: Mail, Messages, Safari data, other apps' data, and so on.
// The privacy database itself is one such file, so trying to open it
// tells. Root does not help: this needs the user's consent in System
// Settings, under Privacy & Security, Full Disk Access.
func fullDiskAccess() *bool {
	f, err := os.Open("/Library/Application Support/com.apple.TCC/TCC.db")
	var ok bool
	switch {
	case err == nil:
		f.Close()
		ok = true
	case errors.Is(err, os.ErrPermission):
		ok = false
	default:
		return nil // cannot tell
	}
	return &ok
}
