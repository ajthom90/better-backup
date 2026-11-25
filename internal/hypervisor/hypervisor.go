// Package hypervisor is what the kinds of virtual machine host have in
// common. A Driver lists a host's machines, freezes one so its disks
// can be read while it keeps running, and builds a machine again from
// a backup. The drivers themselves live in their own packages and are
// gathered by package hypervisor/all.
package hypervisor

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/ajthom90/better-backup/internal/backup"
)

// Options says how a machine is frozen for a backup.
type Options struct {
	// Tracking asks for the hypervisor's change tracking, so that the
	// next backup can skip what did not change.
	Tracking bool
	// Previous is the configuration the machine's last backup recorded
	// (Frozen.Config), or nil.
	Previous []byte
	// Tag names this backup; it shows up in the names of the snapshots
	// and checkpoints the hypervisor is asked to make.
	Tag string
	// Scope keeps the change tracking of different policies apart.
	Scope string
	// StateDir is a folder where a driver may remember things between
	// backups.
	StateDir string
	// ScratchDir, when set, is where a driver that needs scratch space
	// during a backup puts it, instead of its usual place.
	ScratchDir string
	// Connection says how to reach a hypervisor that is managed over
	// the network. It is nil for the host the agent runs on.
	Connection *Connection
}

// Connection is the address of a hypervisor's management service and
// an account on it.
type Connection struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Thumbprint pins the service's TLS certificate (SHA-1 or SHA-256,
	// as hex pairs). Empty checks it against the system's authorities.
	Thumbprint string `json:"thumbprint,omitempty"`
	// Insecure accepts any certificate.
	Insecure bool `json:"insecure,omitempty"`
}

// Frozen is a machine whose disks hold still until Release.
type Frozen struct {
	// Config describes the machine well enough to build it again. It is
	// stored in the snapshot under the driver's ConfigFile name.
	Config []byte
	// Disks are the machine's disks. Each Name is the name its image
	// gets in the snapshot.
	Disks    []backup.ImageSource
	Warnings []string
	// Check, if set, is called after the disks were read and before
	// Release. It returns an error if the frozen view did not hold, in
	// which case what was read is thrown away.
	Check func() error
	// Release lets the machine's disks go: it removes the checkpoint or
	// ends the backup job. It is called once, after the disks are read.
	Release func() error
}

// Target says where a restored machine goes.
type Target struct {
	// Place is where the machine's disks go. What it names depends on
	// the hypervisor: a folder on the host, or a storage.
	Place string
	// Name is the new machine's name.
	Name string
	// Folder names the folder under Place that drivers which make one
	// for a machine's files use. Empty: one named after the machine.
	Folder string
	// Replace says a machine called Name may exist now and will have
	// been set aside (see Replacer) by the time Finish is called.
	Replace bool
	// OpenFile creates a disk image file the way a plain image restore
	// would; drivers whose disks are ordinary files use it.
	OpenFile func(path string, size int64) (w io.WriterAt, fresh bool, done func() error, err error)
	// Connection is as in Options.
	Connection *Connection
}

// DiskSpec describes a disk image about to be restored.
type DiskSpec struct {
	// Name is the image's name in the snapshot.
	Name string
	Size int64
	// The image arrives as blocks of BlockSize bytes, in ascending
	// order; Data says which blocks hold data. The others are holes
	// and are not written to a destination that reads as zeros.
	BlockSize int64
	Data      []bool
}

// Restore builds one machine. Disk is called for every disk image in
// the backup, then Finish.
type Restore interface {
	// Disk returns where a disk's contents are written. fresh says the
	// destination already reads as zeros; done is called after the last
	// write.
	Disk(d DiskSpec) (w io.WriterAt, fresh bool, done func() error, err error)
	// Finish builds the machine around the restored disks and leaves it
	// switched off. It returns warnings about what could not be put back.
	Finish() ([]string, error)
	// Where says where the disks went, for messages.
	Where() string
}

// Aborter is a Restore that has something to undo when the restore
// fails before Finish.
type Aborter interface{ Abort() }

// Replacer is a Driver that can restore a machine over the one that
// has its name. The machine that is there is not deleted: it is
// switched off and renamed, after the disks of the backup have been
// restored, so that a restore that fails or disappoints loses nothing.
type Replacer interface {
	// Find says whether a machine called name exists and, if it does,
	// where a restored copy's files should go: what Target.Place takes.
	Find(ctx context.Context, conn *Connection, name string) (place string, found bool, err error)
	// SetAside switches the machine off, if it runs, and renames it.
	SetAside(ctx context.Context, conn *Connection, name, aside string) error
	// PutBack gives a machine that was set aside its name back. It
	// stays switched off.
	PutBack(ctx context.Context, conn *Connection, name, aside string) error
}

// FolderFor is the folder a machine's files go into under t.Place.
func (t Target) FolderFor() string {
	if t.Folder != "" {
		return SafeName(t.Folder)
	}
	return SafeName(t.Name)
}

// Driver backs up and restores the machines of one kind of hypervisor.
type Driver interface {
	// Kind is the short name used in policies, such as "hyperv".
	Kind() string
	// Label is the name people know it by, such as "Hyper-V".
	Label() string
	// ConfigFile names the entry that holds a machine's configuration in
	// a snapshot. It is how a restore knows which driver made a backup.
	ConfigFile() string
	// Available returns nil when this computer is such a host, or the
	// hypervisor is one that is reached over the network.
	Available() error
	// List names the machines on this host, or on the one conn leads
	// to.
	List(ctx context.Context, conn *Connection) ([]string, error)
	// Freeze prepares a machine to be read.
	Freeze(ctx context.Context, vm string, o Options) (*Frozen, error)
	// NewRestore starts building a machine from the configuration a
	// backup recorded. It should fail early, before any disk is
	// restored, if the machine cannot be built.
	NewRestore(ctx context.Context, config []byte, t Target) (Restore, error)
}

var (
	mu      sync.Mutex
	drivers []Driver
)

// Register adds a driver. Drivers call it when their package loads.
func Register(d Driver) {
	mu.Lock()
	defer mu.Unlock()
	for i, o := range drivers {
		if o.Kind() == d.Kind() {
			drivers[i] = d
			return
		}
	}
	drivers = append(drivers, d)
	sort.SliceStable(drivers, func(i, j int) bool { return drivers[i].Kind() < drivers[j].Kind() })
}

// All returns the registered drivers.
func All() []Driver {
	mu.Lock()
	defer mu.Unlock()
	return append([]Driver(nil), drivers...)
}

// ByKind finds a driver by its short name.
func ByKind(kind string) Driver {
	for _, d := range All() {
		if d.Kind() == kind {
			return d
		}
	}
	return nil
}

// ByConfigFile finds the driver that wrote a configuration entry.
func ByConfigFile(name string) Driver {
	for _, d := range All() {
		if d.ConfigFile() == name {
			return d
		}
	}
	return nil
}

// auto lists the hypervisors a computer can be a host of, in the order
// they are looked for. Proxmox comes before libvirt: a Proxmox node can
// have libvirt's tools installed, but its machines are not libvirt's.
var auto = []string{"hyperv", "proxmox", "libvirt"}

// For returns the driver for a policy's hypervisor setting. An empty
// kind means the hypervisor this computer is a host of.
func For(kind string) (Driver, error) {
	if kind != "" {
		d := ByKind(kind)
		if d == nil {
			return nil, fmt.Errorf("this agent version does not know the hypervisor %q", kind)
		}
		if err := d.Available(); err != nil {
			return nil, err
		}
		return d, nil
	}
	var labels []string
	for _, k := range auto {
		d := ByKind(k)
		if d == nil {
			continue
		}
		if d.Available() == nil {
			return d, nil
		}
		labels = append(labels, d.Label())
	}
	return nil, fmt.Errorf("this computer is not a virtual machine host this program can back up (%s)", strings.Join(labels, ", "))
}

// SafeName turns a machine name into one that can be a file name.
func SafeName(name string) string {
	return strings.Map(func(c rune) rune {
		if c < ' ' || strings.ContainsRune(`/\:*?"<>|`, c) {
			return '_'
		}
		return c
	}, name)
}
