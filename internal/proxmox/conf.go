package proxmox

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// setting is one "key: value" line of a machine's configuration.
type setting struct{ Key, Value string }

// parseConf reads the output of "qm config". It stops at the first
// section header: sections hold snapshots, not the machine as it is.
func parseConf(conf string) []setting {
	var out []setting
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "[") {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, "#") || strings.ContainsAny(k, " \t") {
			continue
		}
		out = append(out, setting{k, strings.TrimSpace(v)})
	}
	return out
}

func confValue(conf []setting, key string) string {
	for _, s := range conf {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

var driveKey = regexp.MustCompile(`^(ide|sata|scsi|virtio|efidisk|tpmstate)\d+$`)

// drive is a parsed drive line such as
// "scsi0: local-lvm:vm-101-disk-0,iothread=1,size=32G".
type drive struct {
	Key     string
	Volume  string // "local-lvm:vm-101-disk-0", a path, or "none"
	Options string // the rest, without the leading comma
}

func parseDrive(s setting) drive {
	d := drive{Key: s.Key}
	var rest []string
	for i, part := range strings.Split(s.Value, ",") {
		switch {
		case strings.HasPrefix(part, "file="):
			d.Volume = strings.TrimPrefix(part, "file=")
		case i == 0 && !strings.Contains(part, "="):
			d.Volume = part
		default:
			rest = append(rest, part)
		}
	}
	d.Options = strings.Join(rest, ",")
	return d
}

func (d drive) option(name string) string {
	for _, part := range strings.Split(d.Options, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// storage is the storage a volume is on, or "" for a path.
func (d drive) storage() string {
	if strings.HasPrefix(d.Volume, "/") {
		return ""
	}
	s, _, ok := strings.Cut(d.Volume, ":")
	if !ok {
		return ""
	}
	return s
}

// format is the image format of a volume, told by its name the way
// Proxmox does.
func volumeFormat(volume string) string {
	switch {
	case strings.HasSuffix(volume, ".qcow2"):
		return "qcow2"
	case strings.HasSuffix(volume, ".vmdk"):
		return "vmdk"
	}
	return "raw"
}

// disks sorts a configuration's drives into the ones to back up and
// warnings about the ones that cannot be.
func disks(conf []setting) (out []Disk, warnings []string) {
	for _, s := range conf {
		if !driveKey.MatchString(s.Key) {
			continue
		}
		d := parseDrive(s)
		switch {
		case d.Volume == "" || d.Volume == "none" || d.option("media") == "cdrom":
			// Removable media and cloud-init drives are not part of
			// the machine's data.
		case strings.HasPrefix(s.Key, "tpmstate"):
			warnings = append(warnings, "the contents of its TPM are not backed up; a restored machine gets an empty one")
		case d.option("backup") == "0":
			// Left out of backups by its owner, as Proxmox's own
			// backup would.
		case d.storage() == "":
			warnings = append(warnings, fmt.Sprintf("the disk %s (%s) is passed through from the host and was not backed up", s.Key, d.Volume))
		default:
			out = append(out, Disk{Key: s.Key, File: "disk-" + s.Key, Volume: d.Volume, Storage: d.storage(), Options: d.Options, Format: volumeFormat(d.Volume)})
		}
	}
	return out, warnings
}

// notRestored are settings that describe a running or locked machine
// or its snapshots rather than the machine itself.
var notRestored = map[string]bool{"name": true, "lock": true, "digest": true, "parent": true, "meta": true, "vmgenid": true,
	"vmstate": true, "snaptime": true, "runningmachine": true, "runningcpu": true, "running-nets-host-mtu": true, "snapstate": true}

// restoreSettings turns a saved configuration into the settings to
// give a new machine. volumes maps drive keys to the new volumes of
// the restored disks; storage is where new cloud-init drives go.
func restoreSettings(conf []setting, volumes map[string]string, storage string) (first, rest []setting, warnings []string) {
	for _, s := range conf {
		switch {
		case notRestored[s.Key] || strings.HasPrefix(s.Key, "unused"):
		case driveKey.MatchString(s.Key):
			d := parseDrive(s)
			vol, restored := volumes[s.Key]
			switch {
			case restored:
				v := vol
				if d.Options != "" {
					v += "," + d.Options
				}
				first = append(first, setting{s.Key, v})
			case strings.HasPrefix(s.Key, "tpmstate"):
				warnings = append(warnings, "its TPM was not in the backup; add one in the machine's hardware settings if the guest needs it")
			case d.option("media") == "cdrom" && strings.Contains(d.Volume, "cloudinit"):
				st := storage
				if st == "" {
					st = d.storage()
				}
				first = append(first, setting{s.Key, st + ":cloudinit," + d.Options})
			case d.Volume == "none" || d.option("media") == "cdrom":
				first = append(first, s)
			case d.storage() == "":
				// A disk passed through from the host: it is still
				// there if this is the same host.
				first = append(first, s)
				warnings = append(warnings, fmt.Sprintf("its disk %s is the host's %s, as before; it was not in the backup", s.Key, d.Volume))
			default:
				warnings = append(warnings, fmt.Sprintf("its disk %s (%s) was not in the backup and is missing from the restored machine", s.Key, d.Volume))
			}
		case s.Key == "cipassword":
			// Proxmox never shows it, so the backup holds only stars.
			warnings = append(warnings, "its cloud-init password is not in the backup; set it again")
		case s.Key == "description":
			if v, err := url.PathUnescape(s.Value); err == nil {
				s.Value = v
			}
			rest = append(rest, s)
		default:
			rest = append(rest, s)
		}
	}
	// A restored machine is a copy of an earlier state of itself, which
	// is what a new generation id tells the guest.
	rest = append(rest, setting{"vmgenid", "1"})
	return first, rest, warnings
}
