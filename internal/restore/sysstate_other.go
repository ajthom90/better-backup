//go:build !windows

package restore

import "errors"

type sysRun struct{}

func (r *restorer) sysBegin() error {
	return errors.New("a system state can only be restored on Windows")
}
func (r *restorer) sysFinish() error { return nil }

func (s *sysRun) place(dest string) (string, bool)           { return dest, false }
func (s *sysRun) later(tmp, dest string) error               { return errors.New("unsupported") }
func (s *sysRun) afterDir(dir string, names map[string]bool) {}

// RestartDSRM is for Windows domain controllers.
func RestartDSRM(enter bool) (string, error) {
	return "", errors.New("only a Windows domain controller has a Directory Services Restore Mode")
}
