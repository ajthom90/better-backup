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
	JobOffsite = "offsite"

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

// Hypervisors are the kinds of virtual machine host a policy can name.
var Hypervisors = []string{"hyperv", "libvirt", "proxmox"}

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
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	AgentID  string   `json:"agent_id"`
	Paths    []string `json:"paths"`
	Excludes []string `json:"excludes,omitempty"`
	// Volumes are backed up as block-level images: drive letters such as
	// "C:" on Windows, or block devices such as /dev/vg0/data elsewhere.
	Volumes []string `json:"volumes,omitempty"`
	// VMs are virtual machines to back up from their host, by name. "*"
	// means every machine on the host.
	VMs []string `json:"vms,omitempty"`
	// Hypervisor says what kind of host the machines are on: one of
	// Hypervisors, or empty for whichever the agent's computer is.
	Hypervisor string `json:"hypervisor,omitempty"`
	// VMTracking says how the disks of virtual machines are read: ""
	// reads them in full every time, "on" reads only what the
	// hypervisor's change tracking says was written since the last
	// backup, and "check" reads them in full and warns where the
	// tracking was wrong.
	VMTracking string `json:"vm_tracking,omitempty"`
	// SystemState backs up what Windows needs to bring a machine's
	// identity back: the registry, boot and system files, and on a domain
	// controller the Active Directory database and SYSVOL.
	SystemState bool      `json:"system_state,omitempty"`
	Schedule    Schedule  `json:"schedule"`
	Retention   Retention `json:"retention"`
	// NoSnapshot backs up the live file system instead of a volume
	// snapshot. It only matters on Windows, where snapshots are the
	// default.
	NoSnapshot bool      `json:"no_snapshot,omitempty"`
	Enabled    bool      `json:"enabled"`
	Created    time.Time `json:"created"`
}

type BackupSpec struct {
	Paths       []string     `json:"paths"`
	Excludes    []string     `json:"excludes,omitempty"`
	Volumes     []string     `json:"volumes,omitempty"`
	VMs         []string     `json:"vms,omitempty"`
	Hypervisor  string       `json:"hypervisor,omitempty"`
	VMTracking  string       `json:"vm_tracking,omitempty"`
	SystemState bool         `json:"system_state,omitempty"`
	NoSnapshot  bool         `json:"no_snapshot,omitempty"`
	Parent      []tree.Entry `json:"parent,omitempty"` // set when the job is handed to the agent
}

type RestoreSpec struct {
	SnapshotID string       `json:"snapshot_id"`
	Sources    []tree.Entry `json:"sources,omitempty"`
	SourceOS   string       `json:"source_os,omitempty"`
	Include    []string     `json:"include,omitempty"`
	Target     string       `json:"target,omitempty"` // empty: restore in place
	Overwrite  string       `json:"overwrite,omitempty"`
	VMName     string       `json:"vm_name,omitempty"` // name for a restored virtual machine
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
	// VMName names a restored virtual machine. Default: its old name.
	VMName string `json:"vm_name,omitempty"`
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

// AlertSettings configures email alerts for failed and missed backups.
type AlertSettings struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port,omitempty"` // default: 587 for starttls, 465 for tls, 25 for none
	// Security is "starttls" (the default), "tls" (TLS from the first
	// byte, usually port 465) or "none".
	Security string `json:"security,omitempty"`
	Username string `json:"username,omitempty"`
	// Password is never returned by the server. When saving, leave it
	// empty and set PasswordSet to keep the stored password.
	Password    string   `json:"password,omitempty"`
	PasswordSet bool     `json:"password_set,omitempty"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	// OnWarning also sends mail for backups that finish with warnings,
	// such as skipped files.
	OnWarning bool `json:"on_warning,omitempty"`
}

// OffsiteSettings configures the second copy of the repository: a folder
// on another disk or share, or a bucket in S3-compatible storage.
type OffsiteSettings struct {
	Enabled bool `json:"enabled"`
	// Kind is "folder" or "s3".
	Kind   string `json:"kind"`
	Folder string `json:"folder,omitempty"`

	Endpoint    string `json:"endpoint,omitempty"`
	Region      string `json:"region,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	AccessKey   string `json:"access_key,omitempty"`
	VirtualHost bool   `json:"virtual_host,omitempty"`
	// SecretKey is never returned by the server. When saving, leave it
	// empty and set SecretKeySet to keep the stored key.
	SecretKey    string `json:"secret_key,omitempty"`
	SecretKeySet bool   `json:"secret_key_set,omitempty"`

	// DailyAt is when the copy is brought up to date each day, "HH:MM"
	// in the server's time zone. Empty: only when started by hand.
	DailyAt string `json:"daily_at,omitempty"`
	// Saved is when the settings were last changed.
	Saved time.Time `json:"saved,omitempty"`
}

// OffsiteStatus is the settings plus how the copy is doing.
type OffsiteStatus struct {
	OffsiteSettings
	// LastSuccess is when the copy was last brought fully up to date.
	LastSuccess *time.Time `json:"last_success,omitempty"`
	// LastJob is the most recent copy job, whatever its outcome.
	LastJob *Job `json:"last_job,omitempty"`
}

// OffsiteTest is the answer to a connection test.
type OffsiteTest struct {
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
}
