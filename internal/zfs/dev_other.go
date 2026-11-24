//go:build !unix

package zfs

func sameFS(a, b string) (same, known bool) { return false, false }
