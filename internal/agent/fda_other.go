//go:build !darwin

package agent

// fullDiskAccess is a macOS matter.
func fullDiskAccess() *bool { return nil }
