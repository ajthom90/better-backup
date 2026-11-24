package api

import "time"

// PolicySummary is what an agent is told about one of its own policies,
// to show on the machine it backs up.
type PolicySummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule,omitempty"`
	Paused   bool   `json:"paused,omitempty"`
	// Pending is true while a backup is queued or running.
	Pending bool `json:"pending,omitempty"`
	// The last finished backup.
	LastStatus   string     `json:"last_status,omitempty"`
	LastFinished *time.Time `json:"last_finished,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	// LastSuccess is when a backup last finished without failing.
	LastSuccess *time.Time `json:"last_success,omitempty"`
}

// AgentSummary answers an agent that asks about itself.
type AgentSummary struct {
	Name     string          `json:"name"`
	Policies []PolicySummary `json:"policies"`
}

// BackupNow answers an agent that asked for its backups to run now.
type BackupNow struct {
	Policies int `json:"policies"` // policies this machine has
	Started  int `json:"started"`  // backups queued by this request
}
