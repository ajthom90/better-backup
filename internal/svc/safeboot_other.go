//go:build !windows

package svc

func safeBoot(string, bool) error { return nil }
