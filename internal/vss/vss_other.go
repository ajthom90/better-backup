//go:build !windows

// Package vss takes Volume Shadow Copy snapshots on Windows. On other
// systems it is a stub: backups read the live file system.
package vss

import (
	"errors"
	"time"
)

// ErrUnsupported is returned by Create on systems without VSS.
var ErrUnsupported = errors.New("volume snapshots are only available on Windows")

// Set is a group of snapshots.
type Set struct{}

func Create([]string, time.Duration) (*Set, []string, error) { return nil, nil, ErrUnsupported }
func (s *Set) Path(p string) string                          { return p }
func (s *Set) Volumes() []string                             { return nil }
func (s *Set) Close() error                                  { return nil }

// EnablePrivileges does nothing outside Windows.
func EnablePrivileges() []string { return nil }

// Writers lists the VSS writers; there are none outside Windows.
func Writers(time.Duration) ([]Writer, error) { return nil, ErrUnsupported }
