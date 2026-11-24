//go:build !windows

package main

// webviewAvailable is always true here: macOS has its web view built
// in, and on Linux the app is linked against WebKitGTK.
func webviewAvailable() bool { return true }
