package agent

import (
	"context"
	"time"
)

// UpdateHooks replaces, for tests, the parts of updating that touch the
// machine. Zero fields are left as they are.
type UpdateHooks struct {
	// Install is what the agent believes it was installed with. Setting
	// it also leaves out the check of who signed the installer, which a
	// stand-in file cannot pass.
	Install string
	// FirstCheck, Settle and HealthWait shorten the waits.
	FirstCheck, Settle, HealthWait time.Duration
	// Start stands in for starting the updater.
	Start func(exe string, args []string) error
}

// SetUpdateHooks puts the hooks in place and returns how to undo that.
func SetUpdateHooks(h UpdateHooks) (undo func()) {
	kind, start, first, settle, health, signer := installKindOf, startUpdater, updateFirstCheck, updateSettle, updateHealthWait, checkSigner
	if h.Install != "" {
		installKindOf = func() string { return h.Install }
		checkSigner = func(context.Context, string, string) error { return nil }
	}
	if h.Start != nil {
		startUpdater = h.Start
	}
	if h.FirstCheck != 0 {
		updateFirstCheck = h.FirstCheck
	}
	if h.Settle != 0 {
		updateSettle = h.Settle
	}
	if h.HealthWait != 0 {
		updateHealthWait = h.HealthWait
	}
	return func() {
		installKindOf, startUpdater, updateFirstCheck, updateSettle, updateHealthWait, checkSigner = kind, start, first, settle, health, signer
	}
}
