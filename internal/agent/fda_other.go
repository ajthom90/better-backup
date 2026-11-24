//go:build !darwin

package agent

import "context"

// fullDiskAccess is a macOS matter.
func fullDiskAccess() *bool { return nil }

// FullDiskAccess is a macOS matter.
func FullDiskAccess() *bool { return nil }

func (s *Service) watchFullDiskAccess(context.Context) {}
