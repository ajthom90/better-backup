// Package api defines the JSON types exchanged between the server, the
// agents and the admin CLI, and the TLS setup the clients share.
package api

import (
	"encoding/json"
	"time"

	"github.com/ajthom90/better-backup/internal/hypervisor"
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
	// StatusCancelled: someone stopped the job in the web UI.
	StatusCancelled = "cancelled"
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
	// Recovery marks a machine started from the recovery disc. It exists
	// only to receive a restore and is not a machine to back up.
	Recovery bool `json:"recovery,omitempty"`
}

// Hypervisors are the kinds of virtual machine host a policy can name.
var Hypervisors = []string{"hyperv", "libvirt", "proxmox", "vmware"}

// VMwareServer is a vCenter Server or ESXi host whose machines can be
// backed up. The server keeps the list and hands the address and
// account to the agent that runs a job, with the job.
type VMwareServer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// URL is the address of the management service, such as
	// vcenter.example.com.
	URL      string `json:"url"`
	Username string `json:"username"`
	// Password is never returned by the server. When saving, leave it
	// empty and set PasswordSet to keep the stored password.
	Password    string `json:"password,omitempty"`
	PasswordSet bool   `json:"password_set,omitempty"`
	// Thumbprint is the SHA-256 or SHA-1 thumbprint of the service's
	// certificate, for one that no authority vouches for.
	Thumbprint string `json:"thumbprint,omitempty"`
	// Insecure accepts any certificate.
	Insecure bool `json:"insecure,omitempty"`
}

// M365Tenant is a Microsoft 365 tenant whose mailboxes and OneDrive can
// be backed up, and the app registration that reads them. The server
// keeps the list and hands the sign-in to the agent that runs a job,
// with the job.
type M365Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// TenantID is the directory (tenant) id, or one of its domain names.
	TenantID string `json:"tenant_id"`
	// ClientID is the application (client) id of the app registration.
	ClientID string `json:"client_id"`
	// ClientSecret is never returned by the server. When saving, leave
	// it empty and set SecretSet to keep the stored secret.
	ClientSecret string `json:"client_secret,omitempty"`
	SecretSet    bool   `json:"secret_set,omitempty"`
	// LoginURL and GraphURL are for the national clouds, such as
	// https://login.microsoftonline.us and https://graph.microsoft.us.
	// Empty: the worldwide service.
	LoginURL string `json:"login_url,omitempty"`
	GraphURL string `json:"graph_url,omitempty"`
}

// M365Spec says what a policy backs up from Microsoft 365.
type M365Spec struct {
	// Tenant is the id of the M365Tenant.
	Tenant string `json:"tenant"`
	// Users are sign-in names such as alice@example.com. "*" means
	// every licensed user who is not a guest.
	Users    []string `json:"users"`
	Mail     bool     `json:"mail,omitempty"`
	OneDrive bool     `json:"onedrive,omitempty"`
}

// M365Connection is the sign-in of a tenant, as an agent gets it.
type M365Connection struct {
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	LoginURL     string `json:"login_url,omitempty"`
	GraphURL     string `json:"graph_url,omitempty"`
}

// M365User is an account of a tenant, for choosing whom to back up.
type M365User struct {
	UPN      string `json:"upn"`
	Name     string `json:"name"`
	Licensed bool   `json:"licensed"`
	Guest    bool   `json:"guest,omitempty"`
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
	ID      string `json:"id"`
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
	// AgentIDs are all the machines the policy backs up, each on its
	// own: one job, and one line of snapshots with its own retention,
	// per machine. AgentID is the first of them; a policy saved with
	// only AgentID has that one machine.
	AgentIDs []string `json:"agent_ids,omitempty"`
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
	// VMwareServer is the id of the VMwareServer the machines are on,
	// for the hypervisor "vmware".
	VMwareServer string `json:"vmware_server,omitempty"`
	// VMTracking says how the disks of virtual machines are read: ""
	// reads them in full every time, "on" reads only what the
	// hypervisor's change tracking says was written since the last
	// backup, and "check" reads them in full and warns where the
	// tracking was wrong.
	VMTracking string `json:"vm_tracking,omitempty"`
	// VMScratchDir is a folder on a KVM or Proxmox host where the old
	// contents of what a machine overwrites during its backup are kept
	// until the backup ends. Empty: the host's usual place.
	VMScratchDir string `json:"vm_scratch_dir,omitempty"`
	// SystemState backs up what Windows needs to bring a machine's
	// identity back: the registry, boot and system files, and on a domain
	// controller the Active Directory database and SYSVOL.
	SystemState bool `json:"system_state,omitempty"`
	// BareMetal backs up the whole machine: the layout of the disks it
	// starts from and an image of each of their partitions, which is
	// what it takes to rebuild the machine on blank hardware.
	BareMetal bool `json:"bare_metal,omitempty"`
	// MachineDisks names the disks BareMetal covers: numbers such as "0"
	// on Windows, devices such as /dev/sda elsewhere. Empty: the disk
	// the system starts from.
	MachineDisks []string `json:"machine_disks,omitempty"`
	// TrueNASConfig backs up the configuration of a TrueNAS system:
	// every setting, and the secret seed that decrypts the passwords
	// stored with them.
	TrueNASConfig bool `json:"truenas_config,omitempty"`
	// M365 backs up mailboxes and OneDrive from Microsoft 365. The agent
	// only needs to reach the internet; nothing of it is on the agent.
	M365      *M365Spec `json:"m365,omitempty"`
	Schedule  Schedule  `json:"schedule"`
	Retention Retention `json:"retention"`
	// NoSnapshot backs up the live file system instead of a snapshot.
	// Snapshots are the default where there are any: VSS on Windows,
	// and ZFS for the paths that are on ZFS datasets.
	NoSnapshot bool      `json:"no_snapshot,omitempty"`
	Enabled    bool      `json:"enabled"`
	Created    time.Time `json:"created"`
}

type BackupSpec struct {
	Paths         []string `json:"paths"`
	Excludes      []string `json:"excludes,omitempty"`
	Volumes       []string `json:"volumes,omitempty"`
	VMs           []string `json:"vms,omitempty"`
	Hypervisor    string   `json:"hypervisor,omitempty"`
	VMTracking    string   `json:"vm_tracking,omitempty"`
	VMScratchDir  string   `json:"vm_scratch_dir,omitempty"`
	SystemState   bool     `json:"system_state,omitempty"`
	TrueNASConfig bool     `json:"truenas_config,omitempty"`
	// BareMetal and MachineDisks are as in Policy.
	BareMetal    bool         `json:"bare_metal,omitempty"`
	MachineDisks []string     `json:"machine_disks,omitempty"`
	NoSnapshot   bool         `json:"no_snapshot,omitempty"`
	Parent       []tree.Entry `json:"parent,omitempty"` // set when the job is handed to the agent
	// VMwareServer is as in Policy. Connection is that server's address
	// and account; like Parent it is only set when the job is handed
	// to the agent, and never stored with the job.
	VMwareServer string                 `json:"vmware_server,omitempty"`
	Connection   *hypervisor.Connection `json:"connection,omitempty"`
	// M365 is as in Policy. M365Connection is its tenant's sign-in,
	// set like Connection.
	M365           *M365Spec       `json:"m365,omitempty"`
	M365Connection *M365Connection `json:"m365_connection,omitempty"`
}

type RestoreSpec struct {
	SnapshotID string       `json:"snapshot_id"`
	Sources    []tree.Entry `json:"sources,omitempty"`
	SourceOS   string       `json:"source_os,omitempty"`
	Include    []string     `json:"include,omitempty"`
	Target     string       `json:"target,omitempty"` // empty: restore in place
	Overwrite  string       `json:"overwrite,omitempty"`
	VMName     string       `json:"vm_name,omitempty"` // name for a restored virtual machine
	// VMReplace is as in RestoreRequest.
	VMReplace bool `json:"vm_replace,omitempty"`
	// Disks says where the disks of a whole machine go: "disk0" to a
	// device such as /dev/sda.
	Disks map[string]string `json:"disks,omitempty"`
	// VMwareServer and Connection are as in BackupSpec.
	VMwareServer string                 `json:"vmware_server,omitempty"`
	Connection   *hypervisor.Connection `json:"connection,omitempty"`
	// SystemState, if set, restores the system state over the running
	// system.
	SystemState *SystemStateRestore `json:"system_state,omitempty"`
}

// SystemStateRestore asks for a Windows system state to be put back
// over the running system it came from. Files in use are swapped in
// when Windows restarts.
type SystemStateRestore struct {
	// Registry also replaces the registry at the restart.
	Registry bool `json:"registry,omitempty"`
	// AuthoritativeSYSVOL makes a domain controller's restored SYSVOL
	// the copy its replication partners take over.
	AuthoritativeSYSVOL bool `json:"authoritative_sysvol,omitempty"`
	// AuthoritativeSubtrees are distinguished names in Active Directory
	// whose restored objects win over newer ones on other domain
	// controllers.
	AuthoritativeSubtrees []string `json:"authoritative_subtrees,omitempty"`
	// Restart restarts Windows when the restore is done.
	Restart bool `json:"restart,omitempty"`
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
	// VMReplace restores a virtual machine over the one that has its
	// name: that one is switched off and renamed, not deleted, once the
	// disks from the backup are in place.
	VMReplace bool `json:"vm_replace,omitempty"`
	// Disks restores a whole machine onto disks of the agent: the name
	// of a disk in the backup ("disk0") to a device such as /dev/sda.
	// Everything on those disks is replaced.
	Disks map[string]string `json:"disks,omitempty"`
	// VMwareServer is the id of the VMwareServer a VMware machine is
	// restored to. Default: the one its policy names.
	VMwareServer string `json:"vmware_server,omitempty"`
	// SystemState restores the snapshot's system state over the running
	// system. It needs an empty Target and the machine the backup came
	// from.
	SystemState *SystemStateRestore `json:"system_state,omitempty"`
}

type EnrollToken struct {
	Token       string    `json:"token"`
	Expires     time.Time `json:"expires"`
	Uses        int       `json:"uses"`
	Fingerprint string    `json:"fingerprint"`
}

// Installers lists the agent installers an administrator can download
// when adding a machine.
type Installers struct {
	Hostname string      `json:"hostname"` // the server's own name, to suggest as its address
	Releases string      `json:"releases"` // page that lists every published installer
	Files    []Installer `json:"files"`
}

type Installer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Format  string `json:"format"` // msi, pkg, deb or rpm
	URL     string `json:"url"`    // a path on this server, or a release download
	Size    int64  `json:"size,omitempty"`
}

// ListEntry is one row of a snapshot directory listing.
type ListEntry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Mode    uint32    `json:"mode"`
}

// DownloadRequest asks for a link to a file or folder of a snapshot.
type DownloadRequest struct {
	Path string `json:"path"`
}

// DownloadLink is a link that downloads once, within two minutes, with
// no sign-in: a file as it is, a folder as a zip file.
type DownloadLink struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
}

// SearchHit is a file or folder whose name matched a search, as the
// newest snapshot that holds it has it.
type SearchHit struct {
	SnapshotID string    `json:"snapshot_id"`
	Time       time.Time `json:"time"`
	// Snapshots is how many of the searched snapshots hold the path,
	// and Oldest the time of the oldest of them.
	Snapshots int       `json:"snapshots"`
	Oldest    time.Time `json:"oldest"`
	AgentID   string    `json:"agent_id"`
	Hostname  string    `json:"hostname"`
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mtime"`
}

type SearchResult struct {
	Hits []SearchHit `json:"hits"`
	// Snapshots is how many snapshots were searched.
	Snapshots int `json:"snapshots"`
	// More says there are more matches than are listed, and Partial
	// that the search ran out of time before it had read every snapshot.
	More    bool `json:"more,omitempty"`
	Partial bool `json:"partial,omitempty"`
	// Indexed says the index of file names gave the answer; without it
	// the snapshots themselves were read, which is slower. Behind is how
	// many new snapshots were not in the index yet.
	Indexed bool `json:"indexed"`
	Behind  int  `json:"behind,omitempty"`
}

// SearchIndexStatus describes the index of file names that searches
// are answered from.
type SearchIndexStatus struct {
	// State is "ready", "building" (searches read the snapshots until
	// it is built) or "off" (it could not be opened).
	State string `json:"state"`
	// Snapshots is how many snapshots there are, and Waiting how many
	// of them are not in the index yet.
	Snapshots int `json:"snapshots"`
	Waiting   int `json:"waiting"`
	// Names is how many different files and folders the index holds,
	// and Bytes its size on disk.
	Names int64  `json:"names"`
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
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
	// KeySaved is false until someone has downloaded the repository key.
	KeySaved bool `json:"key_saved"`
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
	// Summary sends a report of all backups: "" (never), "daily" or
	// "weekly" (on Mondays), at SummaryAt ("HH:MM", default 07:00).
	Summary   string `json:"summary,omitempty"`
	SummaryAt string `json:"summary_at,omitempty"`
	// Webhook, if set, receives every alert as a JSON POST that Slack,
	// Microsoft Teams, Discord and similar services understand. It works
	// with or without email.
	Webhook string `json:"webhook,omitempty"`
}

// MonitorSettings are the checks the server runs on itself.
type MonitorSettings struct {
	// VerifyEveryDays reads and checks all stored data this often; 0
	// turns the check off. VerifyAt is when it starts ("HH:MM").
	VerifyEveryDays int    `json:"verify_every_days"`
	VerifyAt        string `json:"verify_at"`
	// DiskWarnPercent warns when less than this share of the
	// repository's disk is free; 0 turns the warning off.
	DiskWarnPercent int `json:"disk_warn_percent"`
}

// Health is the answer to "is everything protected?".
type Health struct {
	// State is "ok", "warning" or "failed": the worst of what follows.
	State    string         `json:"state"`
	Issues   []HealthIssue  `json:"issues"`
	Policies []PolicyHealth `json:"policies"`
	Disk     DiskUsage      `json:"disk"`
	// Usage is one point a day, oldest first, for at most 90 days.
	Usage []UsagePoint `json:"usage"`
	// GrowthPerDay is the average growth of the repository over the
	// last 30 days of Usage, and DaysLeft how long the disk lasts at
	// that rate (0 when it cannot be told).
	GrowthPerDay int64 `json:"growth_per_day"`
	DaysLeft     int   `json:"days_left,omitempty"`
	// LastVerify is when all data was last checked and found good.
	LastVerify *time.Time `json:"last_verify,omitempty"`
	NextVerify *time.Time `json:"next_verify,omitempty"`
	// Jobs counts what finished in the last 24 hours, by status.
	Jobs map[string]int `json:"jobs"`
}

// HealthIssue is one thing that needs attention.
type HealthIssue struct {
	Level string `json:"level"` // "warning" or "failed"
	Text  string `json:"text"`
}

// PolicyHealth is the state of one policy's backups.
type PolicyHealth struct {
	PolicyID string `json:"policy_id"`
	Policy   string `json:"policy"`
	AgentID  string `json:"agent_id"`
	Agent    string `json:"agent"`
	// State is "ok", "warning" (the last backup skipped something),
	// "failed", "overdue", "never" (no backup yet), "running" or
	// "paused" (not scheduled).
	State    string     `json:"state"`
	Detail   string     `json:"detail,omitempty"`
	LastGood *time.Time `json:"last_good,omitempty"`
	// Size is what the newest snapshot holds, New7Days what its backups
	// added to the repository in the last seven days, Snapshots how many
	// are kept.
	Size      int64 `json:"size"`
	New7Days  int64 `json:"new_7_days"`
	Snapshots int   `json:"snapshots"`
}

// DiskUsage describes the disk the repository is on.
type DiskUsage struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
	// Known is false where the server cannot ask the system.
	Known bool `json:"known"`
}

// UsagePoint is the repository's size on one day.
type UsagePoint struct {
	Day    string `json:"day"` // 2006-01-02
	Stored int64  `json:"stored"`
	Free   int64  `json:"free"`
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

	// LockDays, for S3 storage with Object Lock: everything in the copy
	// is locked against deletion and overwriting, with any credentials,
	// for this many days, and the lock is renewed as long as the server
	// keeps the copy up to date. 0: no lock.
	LockDays int `json:"lock_days,omitempty"`
	// LockMode is "compliance" (nobody can lift the lock; the default)
	// or "governance" (an account with a special permission can).
	LockMode string `json:"lock_mode,omitempty"`

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
	// LockedUntil is the date everything in the copy was locked to by
	// the last run that locked.
	LockedUntil *time.Time `json:"locked_until,omitempty"`
}

// OffsiteTest is the answer to a connection test.
type OffsiteTest struct {
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
	// Locks: the bucket can lock what is stored in it.
	Locks bool `json:"locks,omitempty"`
}

// RecoveryConfig tells a machine started from the recovery disc where
// its server is. It carries no secret.
type RecoveryConfig struct {
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint"`
}

// RecoverySession is the server's answer to a recovery disc that
// announces itself. The code is shown on the machine's screen; the
// administrator approves the machine showing that code.
type RecoverySession struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

// RecoveryHardware describes a machine started from the recovery disc.
type RecoveryHardware struct {
	Product     string         `json:"product,omitempty"`
	Firmware    string         `json:"firmware,omitempty"` // "uefi" or "bios"
	Arch        string         `json:"arch,omitempty"`     // "amd64" or "arm64"
	MemoryBytes int64          `json:"memory_bytes,omitempty"`
	CPUs        int            `json:"cpus,omitempty"`
	Disks       []RecoveryDisk `json:"disks"`
	NICs        []RecoveryNIC  `json:"nics,omitempty"`
}

// RecoveryDisk is a disk a restore can go to.
type RecoveryDisk struct {
	Path       string `json:"path"` // /dev/sda
	Model      string `json:"model,omitempty"`
	Size       int64  `json:"size"`
	SectorSize int    `json:"sector_size"`
	// Contents says in a few words what the disk holds now.
	Contents string `json:"contents,omitempty"`
	// Media marks the disk the recovery disc itself was started from.
	Media bool `json:"media,omitempty"`
}

// RecoveryNIC is a network adapter and its addresses.
type RecoveryNIC struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
}

// RecoveryPoll is what a recovery disc reports while it waits.
type RecoveryPoll struct {
	Version  string            `json:"version,omitempty"`
	Status   string            `json:"status,omitempty"`
	Hardware *RecoveryHardware `json:"hardware,omitempty"`
}

// RecoveryReply answers a poll.
type RecoveryReply struct {
	Approved bool `json:"approved"`
	// EnrollToken is handed over once, after approval: the machine uses
	// it to become an agent that can receive the restore.
	EnrollToken string `json:"enroll_token,omitempty"`
	// Command is "reboot", "poweroff" or "rescan".
	Command string `json:"command,omitempty"`
}

// RecoveryMachine is a machine waiting at the recovery disc, as the
// administrator sees it.
type RecoveryMachine struct {
	ID       string            `json:"id"`
	Code     string            `json:"code"`
	Remote   string            `json:"remote"`
	Created  time.Time         `json:"created"`
	LastSeen time.Time         `json:"last_seen"`
	Approved bool              `json:"approved"`
	AgentID  string            `json:"agent_id,omitempty"`
	Version  string            `json:"version,omitempty"`
	Status   string            `json:"status,omitempty"`
	Hardware *RecoveryHardware `json:"hardware,omitempty"`
	// Job is the machine's most recent restore.
	Job *Job `json:"job,omitempty"`
}

// RecoveryStatus is everything the recovery page shows.
type RecoveryStatus struct {
	// ISO describes the recovery disc image the server can hand out
	// for x64 machines, and ISOArm64 the one for ARM64 machines.
	ISO      RecoveryISO       `json:"iso"`
	ISOArm64 RecoveryISO       `json:"iso_arm64"`
	Machines []RecoveryMachine `json:"machines"`
}

// RecoveryISO describes a recovery disc image the server holds.
type RecoveryISO struct {
	Available bool      `json:"available"`
	Size      int64     `json:"size,omitempty"`
	Modified  time.Time `json:"modified,omitempty"`
}

// MachineBackup is a whole-machine backup inside a snapshot: the disk
// layout, without the raw boot sectors, and the size of each image.
type MachineBackup struct {
	Hostname string        `json:"hostname"`
	OS       string        `json:"os"`
	Firmware string        `json:"firmware,omitempty"`
	Arch     string        `json:"arch,omitempty"` // "amd64" or "arm64"; empty in older backups
	Disks    []MachineDisk `json:"disks"`
}

// MachineDisk is one disk of a MachineBackup.
type MachineDisk struct {
	Name       string `json:"name"`
	Device     string `json:"device,omitempty"`
	Model      string `json:"model,omitempty"`
	Size       int64  `json:"size"`
	SectorSize int    `json:"sector_size"`
	Style      string `json:"style"`
	// Needs is the smallest disk the backup fits on.
	Needs      int64              `json:"needs"`
	Partitions []MachinePartition `json:"partitions"`
}

// MachinePartition is one partition of a MachineDisk.
type MachinePartition struct {
	Number int    `json:"number"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
	FS     string `json:"fs,omitempty"`
	Label  string `json:"label,omitempty"`
	Mount  string `json:"mount,omitempty"`
	Saved  bool   `json:"saved"`
}

// Roles, from least to most allowed. A viewer only reads; an operator
// also runs backups and restores and edits policies and agents; an
// administrator also manages users and the server's settings.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// User is a person who signs in to the web UI.
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled,omitempty"`
	// Password is write-only: set it to choose or change the password.
	Password  string     `json:"password,omitempty"`
	Created   time.Time  `json:"created"`
	LastLogin *time.Time `json:"last_login,omitempty"`
	// Token is true for the built-in identity of the admin token.
	Token bool `json:"token,omitempty"`
}

// LoginRequest signs a user in.
type LoginRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Session is the answer to a sign-in: a bearer token for the admin API.
type Session struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
	User    User      `json:"user"`
}

// SetupStatus tells the web UI, before anyone is signed in, what the
// server still needs.
type SetupStatus struct {
	Version string `json:"version"`
	// NeedsSetup: no user exists yet.
	NeedsSetup bool `json:"needs_setup"`
	// NeedsToken: creating the first user needs the admin token, because
	// the server is already in use.
	NeedsToken bool `json:"needs_token,omitempty"`
	// Locked: the repository exists but its key is missing.
	Locked bool `json:"locked,omitempty"`
	// LockedNeedsSignIn: entering the key needs an administrator.
	LockedNeedsSignIn bool `json:"locked_needs_sign_in,omitempty"`
	// CanRecover: the server is new and empty, so it may be replaced by
	// what an offsite copy holds.
	CanRecover bool `json:"can_recover,omitempty"`
	// KeyFromEnv: the repository key is configured outside the server.
	KeyFromEnv bool              `json:"key_from_env,omitempty"`
	Recovery   *RecoveryProgress `json:"recovery,omitempty"`
}

// SetupRequest creates the first administrator.
type SetupRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Token    string `json:"token,omitempty"`
}

// RecoverRequest rebuilds a new server from an offsite copy.
type RecoverRequest struct {
	Offsite OffsiteSettings `json:"offsite"`
	Key     string          `json:"key"`
	// AsOf, for a copy in a bucket with Object Lock: take the copy as it
	// was at this time, ignoring what was deleted or overwritten since.
	AsOf *time.Time `json:"as_of,omitempty"`
}

// UnlockRequest gives a server the key of the repository it found.
type UnlockRequest struct {
	Key string `json:"key"`
}

// RecoveryProgress describes a server recovery that is running or over.
type RecoveryProgress struct {
	Running bool   `json:"running"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
	Error   string `json:"error,omitempty"`
	// Restarting: the download is complete and the server is starting
	// again on what it fetched.
	Restarting bool `json:"restarting,omitempty"`
}

// AuditEntry is one line of the activity log.
type AuditEntry struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	Remote string    `json:"remote,omitempty"`
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
}

// ServerSettings is what the settings page shows and changes.
type ServerSettings struct {
	DataDir  string `json:"data_dir"`
	RepoDir  string `json:"repo_dir"`
	Listen   string `json:"listen"`
	Hostname string `json:"hostname"`
	// FixedTLSNames come from the command line; TLSNames are set here.
	FixedTLSNames []string `json:"fixed_tls_names,omitempty"`
	TLSNames      []string `json:"tls_names"`
	Fingerprint   string   `json:"fingerprint"`
	// KeySource is "file" (repo.key) or "environment".
	KeySource string `json:"key_source"`
	// KeySaved is when someone last downloaded the repository key.
	KeySaved *time.Time `json:"key_saved,omitempty"`
	// StateSaved is when the server's own configuration was last saved
	// into the repository.
	StateSaved *time.Time `json:"state_saved,omitempty"`
	// Port is the port of Listen.
	Port int `json:"port,omitempty"`
	// Container: the server runs in a container, where the folder and
	// the port belong to the container's definition and cannot be
	// changed from inside.
	Container bool `json:"container,omitempty"`
}

// PlaceRequest changes where the repository is kept and the port the
// server listens on. The server starts again to apply it.
type PlaceRequest struct {
	RepoDir  string `json:"repo_dir"`
	Port     int    `json:"port"`
	Password string `json:"password,omitempty"`
}

// ProtectionSettings is how the server guards its backups against
// someone, or something, that wants them gone: ransomware on a backed-up
// machine, or an attacker who has an administrator's password.
type ProtectionSettings struct {
	// LockDays: a snapshot cannot be deleted, by anyone or by a policy's
	// retention rules, until it is this many days old. 0: no lock.
	LockDays int `json:"lock_days"`
	// DeleteDelayDays: a deleted snapshot, whether a person or a
	// retention rule deleted it, stays recoverable this many days before
	// its data is given up. 0: it is gone at once.
	DeleteDelayDays int `json:"delete_delay_days"`
	// Detect: look at every backup for the marks ransomware leaves, and
	// stop forgetting older backups of that policy until someone has
	// looked.
	Detect bool `json:"detect"`
}

// ProtectionStatus is the settings in force and what is waiting.
type ProtectionStatus struct {
	ProtectionSettings
	// Pending are settings that weaken the protection. They take effect
	// at PendingAt, and can be cancelled until then.
	Pending   *ProtectionSettings `json:"pending,omitempty"`
	PendingAt *time.Time          `json:"pending_at,omitempty"`
	PendingBy string              `json:"pending_by,omitempty"`
	// WaitHours is how long a weakening change waits.
	WaitHours int `json:"wait_hours"`
	// Deleted counts the snapshots that are deleted but still
	// recoverable, and Suspicious the backups nobody has looked at yet.
	Deleted    int `json:"deleted"`
	Suspicious int `json:"suspicious"`
}

// ProtectionRequest changes the protection settings. Password is the
// signed-in user's own password, typed again.
type ProtectionRequest struct {
	ProtectionSettings
	Password string `json:"password,omitempty"`
}

// DeletedSnapshot is a snapshot that was deleted and can still be
// brought back.
type DeletedSnapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	PolicyID string    `json:"policy_id,omitempty"`
	Policy   string    `json:"policy,omitempty"`
	Paths    []string  `json:"paths"`
	Bytes    int64     `json:"bytes"`
	Deleted  time.Time `json:"deleted"`
	// By is who deleted it: a user's name, or "retention".
	By string `json:"by"`
	// Purge is when its data is given up.
	Purge time.Time `json:"purge"`
	// Held: its policy has a backup that looks like ransomware, so it is
	// kept past Purge until someone has looked.
	Held bool `json:"held,omitempty"`
}

// Suspicion is a backup that looks as if ransomware had been at the
// files it holds.
type Suspicion struct {
	SnapshotID string    `json:"snapshot_id"`
	PolicyID   string    `json:"policy_id,omitempty"`
	Policy     string    `json:"policy,omitempty"`
	AgentID    string    `json:"agent_id"`
	Agent      string    `json:"agent,omitempty"`
	Time       time.Time `json:"time"`
	// Reasons say, in sentences, what was found.
	Reasons []string `json:"reasons"`
	// LastGood is the newest snapshot of the policy from before this one.
	LastGood string `json:"last_good,omitempty"`
	// ReviewedBy and Reviewed are set once someone said it is fine.
	ReviewedBy string     `json:"reviewed_by,omitempty"`
	Reviewed   *time.Time `json:"reviewed,omitempty"`
}

// Kinds of things an agent can list on its machine for the web UI.
const (
	BrowseFolders = "folders" // the folders and files under Path
	BrowseVolumes = "volumes" // volumes and block devices that can be imaged
	BrowseDisks   = "disks"   // whole disks, for a whole-machine backup
	BrowseVMs     = "vms"     // virtual machines of a hypervisor
	BrowseLog     = "log"     // not a list: the agent's newest log lines, in Text
)

// BrowseRequest asks an agent what is on its machine, so that someone
// writing a policy can pick instead of type.
type BrowseRequest struct {
	Kind string `json:"kind"`
	// Path is the folder to list; empty asks for where to start (the
	// drives on Windows, / elsewhere).
	Path string `json:"path,omitempty"`
	// Hypervisor and VMwareServer are as in a policy, for BrowseVMs.
	Hypervisor   string `json:"hypervisor,omitempty"`
	VMwareServer string `json:"vmware_server,omitempty"`
	// Connection is filled in by the server for the agent.
	Connection *hypervisor.Connection `json:"connection,omitempty"`
}

// BrowseEntry is one thing found. Value is what goes into the policy.
type BrowseEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Dir    bool   `json:"dir,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// BrowseResult is an agent's answer.
type BrowseResult struct {
	// Path is the folder that was listed, as the machine spells it.
	Path string `json:"path,omitempty"`
	// Up is the folder above it; nil at the top.
	Up        *string       `json:"up,omitempty"`
	Entries   []BrowseEntry `json:"entries"`
	Truncated bool          `json:"truncated,omitempty"`
	Error     string        `json:"error,omitempty"`
	// Text is the answer to BrowseLog.
	Text string `json:"text,omitempty"`
}

// AgentQuery is a question the server asks an agent outside its jobs;
// the agent answers at once, also while a job runs.
type AgentQuery struct {
	ID     string         `json:"id"`
	Browse *BrowseRequest `json:"browse,omitempty"`
	// Action is something to do at once instead; the answer's Text says
	// what was done.
	Action string `json:"action,omitempty"`
}

// Actions an agent can be asked for outside a job.
const (
	// ActionEnterDSRM restarts a domain controller into Directory
	// Services Restore Mode, where Active Directory can be restored.
	ActionEnterDSRM = "dsrm-enter"
	// ActionLeaveDSRM restarts it back into normal operation.
	ActionLeaveDSRM = "dsrm-leave"
)

// Machines returns the agents a policy backs up.
func (p *Policy) Machines() []string {
	if len(p.AgentIDs) > 0 {
		return p.AgentIDs
	}
	if p.AgentID != "" {
		return []string{p.AgentID}
	}
	return nil
}

// Covers says whether the policy backs up this agent's machine.
func (p *Policy) Covers(agentID string) bool {
	for _, id := range p.Machines() {
		if id == agentID {
			return true
		}
	}
	return false
}
