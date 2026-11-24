//go:build !linux

package recovery

import (
	"fmt"
	"os"
)

// Main runs the recovery disc, which is a Linux system.
func Main(version string) {
	fmt.Fprintln(os.Stderr, "bb-agent recover is the program on the better-backup recovery disc; it only runs there")
	os.Exit(2)
}
