//go:build !windows

package server

func openFirewall(int) error { return nil }
