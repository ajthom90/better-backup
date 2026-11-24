//go:build !linux

package agent

import "errors"

// EnterHostRoot is only possible on Linux.
func EnterHostRoot(string) error {
	return errors.New("BB_HOST_ROOT is only supported on Linux")
}
