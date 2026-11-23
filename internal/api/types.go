// Package api defines the JSON types exchanged between the server, the
// agents and the admin CLI, and the TLS setup the clients share.
package api

import (
	"encoding/json"
	"time"

	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

// Job types and states.
const (
	JobBackup  = "backup"
	JobRestore = "restore"
	JobGC      = "gc"
	JobVerify  = "verify"

	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusWarning = "warning" // finished, but some files were skipped
	StatusFailed  = "failed"
)

type EnrollRequest struct {
	Token    string `json:"token"`
	CSR      string `json:"csr"` // PEM certificate request
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
}

type EnrollResponse struct {
	AgentID string `json:"agent_id"`
	Cert    string `json:"cert"`   // PEM client certificate
	IDKey   []byte `json:"id_key"` // keys the chunk-id hash
}

type Agent struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Hostname   string    `json:"hostname"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	Version    string    `json:"version"`
	EnrolledAt time.Time `json:"enrolled_at"`
	LastSeen   time.Time `json:"last_seen"`
	Online     bool      `json:"online"`
	Revoked    bool      `json:"revoked"`
}

type Schedule struct {
	Interval string `json:"interval,omitempty"` // Go duration, e.g. "24h"
	DailyAt  string `json:"daily_at,omitempty"` // "HH:MM" in server local time
}

type Retention struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	KeepYearly  int `json:"keep_yearly,omitempty"`
}

type Policy struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	AgentID   string    `json:"agent_id"`
	Paths     []string  `json:"paths"`
	Excludes  []string  `json:"excludes,omitempty"`
	Schedule  Schedule  `json:"schedule"`
	Retention Retention `json:"retention"`
	// NoSnapshot backs up the live file system instead of a volume
	// snapshot. It only matters on Windows, where snapshots are the
	// default.
	NoSnapshot bool      `json:"no_snapshot,omitempty"`
	Enabled    bool      `json:"enabled"`
	Created    time.Time `json:"created"`
}

type BackupSpec struct {
	Paths      []string     `json:"paths"`
	Excludes   []string     `json:"excludes,omitempty"`
	NoSnapshot bool         `json:"no_snapshot,omitempty"`
	Parent     []tree.Entry `json:"parent,omitempty"` // set when the job is handed to the agent
}

type RestoreSpec struct {
	SnapshotID string       `json:"snapshot_id"`
	Sources    []tree.Entry `json:"sources,omitempty"`
	SourceOS   string       `json:"source_os,omitempty"`
	Include    []string     `json:"include,omitempty"`
	Target     string       `json:"target,omitempty"` // empty: restore in place
	Overwrite  string       `json:"overwrite,omitempty"`
}

type Job struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	AgentID    string          `json:"agent_id,omitempty"`
	PolicyID   string          `json:"policy_id,omitempty"`
	SnapshotID string          `json:"snapshot_id,omitempty"`
	Backup     *BackupSpec     `json:"backup,omitempty"`
	Restore    *RestoreSpec    `json:"restore,omitempty"`
	Progress   json.RawMessage `json:"progress,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	Error      string          `json:"error,omitempty"`
	Created    time.Time       `json:"created"`
	Started    *time.Time      `json:"started,omitempty"`
	Finished   *time.Time      `json:"finished,omitempty"`
}

// CompleteRequest is an agent's final report for a job.
type CompleteRequest struct {
	Error    string         `json:"error,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	Sources  []tree.Entry   `json:"sources,omitempty"`
	Stats    *tree.Stats    `json:"stats,omitempty"`
	Restore  *restore.Stats `json:"restore,omitempty"`
}

type RestoreRequest struct {
	SnapshotID string   `json:"snapshot_id"`
	AgentID    string   `json:"agent_id,omitempty"` // default: the agent that was backed up
	Include    []string `json:"include,omitempty"`
	Target     string   `json:"target,omitempty"`
	Overwrite  string   `json:"overwrite,omitempty"`
}

type EnrollToken struct {
	Token       string    `json:"token"`
	Expires     time.Time `json:"expires"`
	Uses        int       `json:"uses"`
	Fingerprint string    `json:"fingerprint"`
}

// ListEntry is one row of a snapshot directory listing.
type ListEntry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Mode    uint32    `json:"mode"`
}

type Status struct {
	Version     string `json:"version"`
	RepoID      string `json:"repo_id"`
	Fingerprint string `json:"fingerprint"`
	Agents      int    `json:"agents"`
	Snapshots   int    `json:"snapshots"`
	Packs       int64  `json:"packs"`
	StoredBytes int64  `json:"stored_bytes"`
	RawBytes    int64  `json:"raw_bytes"`
	Chunks      int64  `json:"chunks"`
	RunningJobs int    `json:"running_jobs"`
}

type Error struct {
	Error string `json:"error"`
}

// MaxWarnings caps the warning messages kept per job.
const MaxWarnings = 200
