//go:build unix

package restore

import (
	"os"

	"github.com/ajthom90/better-backup/internal/tree"
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

func (r *restorer) platformMeta(string, *tree.Entry) {}
