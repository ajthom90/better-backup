package server

import (
	"fmt"
	"os/exec"
)

// openFirewall lets the new port through Windows Firewall, as the
// installer did for the first one.
func openFirewall(port int) error {
	out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		fmt.Sprintf("name=better-backup server (port %d)", port), "dir=in", "action=allow", "protocol=TCP", fmt.Sprintf("localport=%d", port)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
