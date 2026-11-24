//go:build unix

package restore

import (
	"os"
	"runtime"
	"strings"

	"github.com/ajthom90/better-backup/internal/tree"
	"github.com/ajthom90/better-backup/internal/xattr"
)

// chown restores ownership. Only root can, and Windows backups carry no
// Unix owner, so both cases are skipped.
func (r *restorer) chown(path string, e *tree.Entry) {
	if os.Geteuid() != 0 || r.opts.SourceOS == "windows" {
		return
	}
	if err := os.Lchown(path, int(e.UID), int(e.GID)); err != nil {
		r.warn("%v", err)
	}
}

// platformMeta restores extended attributes, and with them access
// control lists. They mean something only on the kind of system they
// came from.
func (r *restorer) platformMeta(path string, e *tree.Entry) {
	if len(e.Xattrs) == 0 || r.opts.SourceOS != runtime.GOOS {
		return
	}
	if failed, err := xattr.Write(path, e.Xattrs); err != nil {
		// One message per kind of failure: a target that cannot hold
		// an attribute cannot hold it for any file.
		key := strings.Join(failed, ", ") + ": " + err.Error()
		if r.xattrWarned == nil {
			r.xattrWarned = map[string]bool{}
		}
		if !r.xattrWarned[key] {
			r.xattrWarned[key] = true
			r.warn("%s: could not restore extended attributes %s: %v (other files with the same problem are not listed)", path, strings.Join(failed, ", "), err)
		}
	}
}
