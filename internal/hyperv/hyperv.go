// Package hyperv backs up and restores Hyper-V virtual machines from
// the host. It drives Hyper-V through its PowerShell module: a
// checkpoint freezes the machine's disks so they can be read while it
// keeps running, and a restore builds a new machine around restored
// disks.
package hyperv

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported is returned on systems that are not Hyper-V hosts.
var ErrUnsupported = errors.New("Hyper-V backup needs a Windows host with the Hyper-V role")

// ConfigFile is the name of the entry that holds a machine's Config in
// a snapshot.
const ConfigFile = "vm.json"

// Config describes a virtual machine well enough to build it again.
type Config struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Generation     int    `json:"generation"`
	Version        string `json:"version,omitempty"`
	State          string `json:"state,omitempty"` // when backed up
	Processors     int    `json:"processors"`
	MemoryStartup  int64  `json:"memory_startup"`
	DynamicMemory  bool   `json:"dynamic_memory"`
	MemoryMin      int64  `json:"memory_min,omitempty"`
	MemoryMax      int64  `json:"memory_max,omitempty"`
	SecureBoot     bool   `json:"secure_boot"`
	SecureTemplate string `json:"secure_boot_template,omitempty"`
	CheckpointType string `json:"checkpoint_type,omitempty"`
	Disks          []Disk `json:"disks"`
	NICs           []NIC  `json:"nics"`
	Notes          string `json:"notes,omitempty"`
}

// Disk is one virtual hard disk and where it is attached.
type Disk struct {
	ControllerType     string `json:"controller_type"` // IDE or SCSI
	ControllerNumber   int    `json:"controller_number"`
	ControllerLocation int    `json:"controller_location"`
	// Path is the disk's file on the host when it was backed up.
	Path string `json:"path"`
	// File is the name of the disk's image inside the snapshot.
	File string `json:"file,omitempty"`
	// TrackingID names the moment of this backup in the disk's change
	// tracking. The next backup asks the disk what was written since.
	TrackingID string `json:"tracking_id,omitempty"`

	// Tracked says Changed lists everything written to the disk since
	// the previous backup.
	Tracked bool    `json:"-"`
	Changed []Range `json:"-"`
}

// Range is a run of bytes of a disk's contents.
type Range struct{ Off, Len int64 }

// Options says how Prepare freezes a machine.
type Options struct {
	// Tracking asks Hyper-V to track changes to the machine's disks, so
	// that the next backup can skip what did not change.
	Tracking bool
	// Previous is the Config of the machine's last backup, if any.
	Previous *Config
	// StateDir is a folder where the reference points this program made
	// are remembered, so they can be removed when they are replaced.
	StateDir string
}

// NIC is one network adapter.
type NIC struct {
	Name       string `json:"name"`
	Switch     string `json:"switch,omitempty"`
	MAC        string `json:"mac,omitempty"`
	DynamicMAC bool   `json:"dynamic_mac"`
}

// FileName is the name a disk's image gets in a snapshot.
func (d Disk) FileName() string {
	return fmt.Sprintf("disk-%s-%d-%d.vhdx", strings.ToLower(d.ControllerType), d.ControllerNumber, d.ControllerLocation)
}

// isDiskFile reports whether a drive's path is a virtual disk file, as
// opposed to a physical disk passed through to the machine.
func isDiskFile(path string) bool {
	p := strings.ToLower(path)
	for _, ext := range []string{".vhdx", ".avhdx", ".vhd", ".avhd"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}
