package agent

import (
	"fmt"
	"os"
	"syscall"
)

// EnterHostRoot makes root this process's file system. An agent that
// runs in a container, as an app on a NAS for example, is given the
// host's root (/proc/1/root, with the host's process ids and full
// privileges) and from then on sees the host's files, devices and
// programs at their real paths: a backup records the paths the host
// knows, a restore in place writes to them, and the host's own zfs
// command takes the snapshots.
func EnterHostRoot(root string) error {
	if err := syscall.Chroot(root); err != nil {
		return fmt.Errorf("entering the host's file system at %s: %w (the container must be privileged and share the host's process ids)", root, err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	// The container's image may have no PATH worth the name.
	return os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
}
