//go:build !linux && !darwin

package xattr

// Supported says whether this system has extended attributes that this
// package reads.
const Supported = false

func Read(string) (map[string][]byte, []string, error) { return nil, nil, nil }

func Write(string, map[string][]byte) ([]string, error) { return nil, nil }
