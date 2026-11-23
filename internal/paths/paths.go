// Package paths gives the default locations of server and agent state.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

func base(name, env string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, name)
	case "darwin":
		return filepath.Join("/Library/Application Support", name)
	}
	return filepath.Join("/var/lib", name)
}

// ServerData is the server's data directory ($BB_DATA).
func ServerData() string { return base("better-backup", "BB_DATA") }

// AgentDir is the agent's state directory ($BB_AGENT_DIR).
func AgentDir() string { return base("better-backup-agent", "BB_AGENT_DIR") }
