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
	// OpenFile creates a disk image file the way a plain image restore
	// would; drivers whose disks are ordinary files use it.
	OpenFile func(path string, size int64) (w io.WriterAt, fresh bool, done func() error, err error)
}

// Restore builds one machine. Disk is called for every disk image in
// the backup, then Finish.
type Restore interface {
	// Disk returns where a disk's contents are written. fresh says the
	// destination already reads as zeros; done is called after the last
	// write.
	Disk(name string, size int64) (w io.WriterAt, fresh bool, done func() error, err error)
	// Finish builds the machine around the restored disks and leaves it
	// switched off. It returns warnings about what could not be put back.
	Finish() ([]string, error)
	// Where says where the disks went, for messages.
	Where() string
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
	// Available returns nil when this computer is such a host.
	Available() error
	// List names the machines on this host.
	List(ctx context.Context) ([]string, error)
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
	for _, d := range All() {
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
