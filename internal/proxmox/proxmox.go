// Package proxmox backs up and restores the virtual machines of a
// Proxmox VE node. It uses the node's own tools: qm for machines and
// their settings, pvesm for storage, and the QMP socket Proxmox keeps
// for every running machine to read its disks while it runs (see
// live.go). A machine that is switched off is read from its volumes.
// Containers are not virtual machines and are not covered.
package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/nbd"
	"github.com/ajthom90/better-backup/internal/qemuimg"
)

// ConfigFile is the name of the entry that holds a machine's Config in
// a snapshot.
const ConfigFile = "proxmox.json"

// Config describes a machine well enough to create it again.
type Config struct {
	VMID  int    `json:"vmid"`
	Name  string `json:"name"`
	State string `json:"state,omitempty"` // when backed up
	// Conf is the machine's configuration as "qm config" prints it.
	Conf string `json:"conf"`
	// Bitmap names the dirty bitmap this backup started in the running
	// QEMU. The next backup asks what was written since.
	Bitmap string `json:"bitmap,omitempty"`
	Disks  []Disk `json:"disks"`
}

// Disk is one of a machine's disks.
type Disk struct {
	// Key is the disk's place in the machine, such as "scsi0".
	Key string `json:"key"`
	// File is the name of the disk's image inside the snapshot.
	File string `json:"file"`
	// Volume is the disk's volume when backed up, such as
	// "local-lvm:vm-101-disk-0".
	Volume  string `json:"volume"`
	Storage string `json:"storage"`
	// Options are the drive's settings other than its volume.
	Options string `json:"options,omitempty"`
	Format  string `json:"format"`
	Size    int64  `json:"size,omitempty"`
}

func init() { hypervisor.Register(driver{}) }

type driver struct{}

func (driver) Kind() string       { return "proxmox" }
func (driver) Label() string      { return "Proxmox VE" }
func (driver) ConfigFile() string { return ConfigFile }

// root is prepended to the fixed paths of a Proxmox node. It is empty
// except in tests, which stand in for a node with a folder.
func root() string { return os.Getenv("BB_PVE_ROOT") }

func (driver) Available() error {
	if runtime.GOOS != "linux" {
		return errors.New("Proxmox backup needs a Proxmox VE node")
	}
	if _, err := os.Stat(root() + "/etc/pve"); err != nil {
		return errors.New("this computer is not a Proxmox VE node (/etc/pve is missing)")
	}
	for _, tool := range []string{"qm", "pvesm", "pvesh"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("this computer is not a Proxmox VE node (%s is missing)", tool)
		}
	}
	return qemuimg.Available()
}

// run runs one of Proxmox's tools and returns what it printed.
func run(ctx context.Context, tool string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(errb.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = strings.TrimSpace(msg[i+1:])
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", tool, args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// machine is one row of "qm list".
type machine struct {
	ID     int
	Name   string
	Status string
}

func machines(ctx context.Context) ([]machine, error) {
	out, err := run(ctx, "qm", "list")
	if err != nil {
		return nil, err
	}
	var ms []machine
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue // the header
		}
		ms = append(ms, machine{ID: id, Name: f[1], Status: f[2]})
	}
	return ms, nil
}

// List names the machines on this node. A machine whose name another
// one shares, or that has none, is listed by its id.
func (driver) List(ctx context.Context, _ *hypervisor.Connection) ([]string, error) {
	ms, err := machines(ctx)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, m := range ms {
		count[m.Name]++
	}
	var names []string
	for _, m := range ms {
		if m.Name == "" || m.Name == "-" || count[m.Name] > 1 || isNumber(m.Name) {
			names = append(names, strconv.Itoa(m.ID))
		} else {
			names = append(names, m.Name)
		}
	}
	return names, nil
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// find resolves what a policy calls a machine: its id or its name.
func find(ctx context.Context, vm string) (machine, error) {
	ms, err := machines(ctx)
	if err != nil {
		return machine{}, err
	}
	if id, err := strconv.Atoi(vm); err == nil {
		for _, m := range ms {
			if m.ID == id {
				return m, nil
			}
		}
	}
	var found []machine
	for _, m := range ms {
		if m.Name == vm {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return machine{}, errors.New("there is no such virtual machine on this node")
	}
	return machine{}, fmt.Errorf("%d machines on this node have this name; name it by its id instead", len(found))
}

func bitmapPrefix(scope string) string {
	if scope == "" {
		scope = "manual"
	}
	return "bb-" + clean(scope) + "-"
}

func clean(s string) string {
	return strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			return c
		}
		return '_'
	}, s)
}

// scratchDir is where the old contents of what a machine overwrites
// during its backup are kept until the backup ends.
func scratchDir(chosen string) string {
	if chosen != "" {
		return chosen
	}
	if d := os.Getenv("BB_SCRATCH_DIR"); d != "" {
		return d
	}
	return "/var/tmp"
}

func (d driver) Freeze(ctx context.Context, vm string, o hypervisor.Options) (*hypervisor.Frozen, error) {
	m, err := find(ctx, vm)
	if err != nil {
		return nil, err
	}
	id := strconv.Itoa(m.ID)
	conf, err := run(ctx, "qm", "config", id, "--current", "1")
	if err != nil {
		return nil, fmt.Errorf("reading its configuration: %w", err)
	}
	settings := parseConf(conf)
	if lock := confValue(settings, "lock"); lock != "" {
		return nil, fmt.Errorf("it is locked by another operation (%s); it is backed up next time", lock)
	}
	cfg := Config{VMID: m.ID, Name: m.Name, State: m.Status, Conf: conf}
	f := &hypervisor.Frozen{}
	warn := func(format string, args ...any) {
		f.Warnings = append(f.Warnings, vm+": "+fmt.Sprintf(format, args...))
	}
	var skipped []string
	cfg.Disks, skipped = disks(settings)
	for _, w := range skipped {
		warn("%s", w)
	}
	if len(cfg.Disks) == 0 {
		return nil, errors.New("it has no disks that can be backed up")
	}
	var prev *Config
	if p := new(Config); len(o.Previous) > 0 && json.Unmarshal(o.Previous, p) == nil {
		prev = p
	}
	qmpSocket := filepath.Join(root(), "/var/run/qemu-server", id+".qmp")
	if _, serr := os.Stat(qmpSocket); m.Status == "running" || m.Status == "paused" {
		if serr != nil {
			return nil, fmt.Errorf("it is running but its control socket %s is missing", qmpSocket)
		}
		err = d.freezeLive(ctx, id, qmpSocket, settings, o, &cfg, prev, f, warn)
	} else {
		// Read in full, even with change tracking: the record of
		// changes lived in the QEMU that is no longer running.
		err = d.freezeOff(ctx, &cfg, f)
	}
	if err != nil {
		return nil, err
	}
	for i := range f.Disks {
		cfg.Disks[i].Size = f.Disks[i].Size
	}
	if f.Config, err = json.Marshal(cfg); err != nil {
		for _, s := range f.Disks {
			s.Close()
		}
		return nil, errors.Join(err, f.Release())
	}
	return f, nil
}

// source turns an open export into something the archiver reads. Parts
// the server says read as zeros are not read at all.
func source(name string, c *nbd.Client, closer func()) (backup.ImageSource, error) {
	src := backup.ImageSource{Name: name, Size: c.Size(), R: c, SkipZero: true, Close: closer}
	if c.Has(nbd.Allocation) {
		ext, err := c.Status(nbd.Allocation)
		if err != nil {
			return src, err
		}
		src.Used = []backup.Extent{}
		for _, e := range ext {
			if e.Flags&2 == 0 {
				src.Used = append(src.Used, backup.Extent{Off: e.Off, Len: e.Len})
			}
		}
	}
	return src, nil
}

// agentEnabled reports whether the machine is set up to use the QEMU
// guest agent ("agent: 1" or "agent: enabled=1,...").
func agentEnabled(settings []setting) bool {
	v := confValue(settings, "agent")
	for _, part := range strings.Split(v, ",") {
		if part == "1" || part == "enabled=1" {
			return true
		}
	}
	return false
}

func (driver) freezeLive(ctx context.Context, id, qmpSocket string, settings []setting, o hypervisor.Options, cfg, prev *Config, f *hypervisor.Frozen, warn func(string, ...any)) error {
	prefix := bitmapPrefix(o.Scope)
	base := map[string]string{}
	if o.Tracking {
		cfg.Bitmap = prefix + clean(o.Tag)
		if prev != nil && prev.Bitmap != "" {
			for _, d := range cfg.Disks {
				for _, p := range prev.Disks {
					if p.Key == d.Key && p.File == d.File {
						base[d.Key] = prev.Bitmap
					}
				}
			}
		}
	}
	var keysWanted []string
	for _, d := range cfg.Disks {
		keysWanted = append(keysWanted, d.Key)
	}
	// Ask the guest to flush its file systems, if it runs the QEMU
	// guest agent. Without one the backup is what a power cut would
	// leave, which file systems and databases are built to survive.
	frozen := false
	if agentEnabled(settings) {
		fctx, stop := context.WithTimeout(ctx, 60*time.Second)
		_, err := run(fctx, "qm", "guest", "cmd", id, "fsfreeze-freeze")
		frozen = err == nil
		stop()
	}
	l, err := startLive(ctx, qmpSocket, keysWanted, scratchDir(o.ScratchDir), prefix, cfg.Bitmap, base)
	if frozen {
		if _, terr := run(context.WithoutCancel(ctx), "qm", "guest", "cmd", id, "fsfreeze-thaw"); terr != nil {
			warn("the guest's file systems were frozen for the backup and could not be released; run 'qm guest cmd %s fsfreeze-thaw' on the node: %v", id, terr)
		}
	}
	if err != nil {
		return fmt.Errorf("freezing its disks: %w", err)
	}
	f.Check = func() error { return l.Check(ctx) }
	f.Release = func() error {
		if err := l.Stop(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("ending its backup jobs failed; they end when the machine is next stopped and started: %w", err)
		}
		return nil
	}
	fail := func(err error) error {
		for _, s := range f.Disks {
			s.Close()
		}
		f.Disks = nil
		// Without a backup there is nothing the new bitmap could be
		// the start of.
		delete(l.keep, cfg.Bitmap)
		return errors.Join(err, f.Release())
	}
	for i := range cfg.Disks {
		dk, ld := &cfg.Disks[i], l.Disks[i]
		contexts := []string{nbd.Allocation}
		if ld.Changes != "" {
			contexts = append(contexts, nbd.DirtyBitmap(ld.Changes))
		}
		c, err := nbd.Dial("unix", l.Socket, dk.Key, contexts...)
		if err != nil {
			return fail(fmt.Errorf("opening its disk %s: %w", dk.Key, err))
		}
		src, err := source(dk.File, c, func() { c.Close() })
		if err != nil {
			c.Close()
			return fail(fmt.Errorf("reading the layout of its disk %s: %w", dk.Key, err))
		}
		if ld.Changes != "" {
			ext, err := c.Status(nbd.DirtyBitmap(ld.Changes))
			if err != nil {
				c.Close()
				return fail(fmt.Errorf("listing the changes of its disk %s: %w", dk.Key, err))
			}
			src.Tracked = true
			for _, e := range ext {
				if e.Flags&1 != 0 {
					src.Changed = append(src.Changed, backup.Extent{Off: e.Off, Len: e.Len})
				}
			}
		}
		f.Disks = append(f.Disks, src)
	}
	return nil
}

// volumePath asks Proxmox where a volume is. For a volume on LVM that
// is not active, as those of a stopped machine are, it activates it;
// deactivate undoes that.
func volumePath(ctx context.Context, volume string) (path string, deactivate func(), err error) {
	deactivate = func() {}
	if path, err = run(ctx, "pvesm", "path", volume); err != nil {
		return "", deactivate, err
	}
	if !strings.HasPrefix(path, "/dev/") || strings.HasPrefix(path, "/dev/zvol/") {
		return path, deactivate, nil
	}
	if _, serr := os.Stat(path); serr == nil {
		return path, deactivate, nil
	}
	// /dev/<group>/<volume>: an LVM volume that is not active.
	lv := strings.TrimPrefix(path, "/dev/")
	if strings.Count(lv, "/") != 1 {
		return "", deactivate, fmt.Errorf("the volume's device %s does not exist", path)
	}
	// -K: thin volumes are marked to be skipped when activating.
	if _, err := run(ctx, "lvchange", "-ay", "-K", lv); err != nil {
		return "", deactivate, fmt.Errorf("activating the volume: %w", err)
	}
	return path, func() { run(context.Background(), "lvchange", "-an", lv) }, nil
}

// freezeOff opens the volumes of a machine that is not running.
func (driver) freezeOff(ctx context.Context, cfg *Config, f *hypervisor.Frozen) error {
	var undo []func()
	release := func() {
		for _, u := range undo {
			u()
		}
	}
	for i := range cfg.Disks {
		dk := &cfg.Disks[i]
		path, deactivate, err := volumePath(ctx, dk.Volume)
		undo = append(undo, deactivate)
		if err == nil {
			var im *qemuimg.Image
			if im, err = qemuimg.Open(ctx, path, dk.Format, false, nbd.Allocation); err == nil {
				var src backup.ImageSource
				if src, err = source(dk.File, im.Client, func() { im.Close() }); err == nil {
					f.Disks = append(f.Disks, src)
					continue
				}
				im.Close()
			}
		}
		for _, s := range f.Disks {
			s.Close()
		}
		release()
		return fmt.Errorf("opening its disk %s (%s): %w", dk.Key, dk.Volume, err)
	}
	f.Release = func() error { release(); return nil }
	return nil
}
