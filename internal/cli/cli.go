// Package cli holds helpers shared by the command-line programs.
package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// Multi is a flag that may be given several times.
type Multi []string

func (m *Multi) String() string     { return strings.Join(*m, ",") }
func (m *Multi) Set(v string) error { *m = append(*m, v); return nil }

// Parse parses flags that may appear before, between or after positional
// arguments, and returns the positionals.
func Parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// Fatal prints an error and exits.
func Fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// Size formats a byte count.
func Size(n int64) string {
	const units = "KMGTPE"
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(1024), 0
	for v := n / 1024; v >= 1024; v /= 1024 {
		div *= 1024
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
}

// Env returns the environment variable or a default.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
