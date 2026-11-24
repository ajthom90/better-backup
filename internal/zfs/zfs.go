// Package zfs lets a backup read from ZFS snapshots: the files of a
// dataset as they were at one instant, and a volume (zvol) frozen the
// same way. It drives the zfs command, so it works wherever OpenZFS is
// installed: TrueNAS SCALE, Proxmox VE, or any Linux with ZFS.
package zfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Command is the zfs program. Tests replace it.
var Command = "zfs"

// Property marks the snapshots and clones this program made, with the
// id of the agent that made them, so that leftovers of a backup that
// was cut short can be found and removed without touching anything else.
const Property = "org.better-backup:agent"

// deviceWait is how long a clone's device node may take to appear.
var deviceWait = 30 * time.Second

func find() (string, error) {
	if p, err := exec.LookPath(Command); err == nil {
		return p, nil
	}
	if !strings.ContainsAny(Command, `/\`) {
		for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin", "/usr/local/bin"} {
			p := filepath.Join(dir, Command)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	return "", errors.New("the zfs command was not found")
}

// Available reports whether this machine has the zfs command.
func Available() bool {
	_, err := find()
	return err == nil
}

func run(ctx context.Context, args ...string) (string, error) {
	bin, err := find()
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("zfs %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// Dataset is a file system or a volume.
type Dataset struct {
	Name       string
	Volume     bool
	Mountpoint string // empty when it has none
}

// List returns every file system and volume of every imported pool.
func List(ctx context.Context) ([]Dataset, error) {
	out, err := run(ctx, "list", "-H", "-p", "-t", "filesystem,volume", "-o", "name,type,mountpoint")
	if err != nil {
		return nil, err
	}
	var ds []Dataset
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		d := Dataset{Name: f[0], Volume: f[1] == "volume"}
		if strings.HasPrefix(f[2], "/") {
			d.Mountpoint = filepath.Clean(f[2])
		}
		ds = append(ds, d)
	}
	return ds, nil
}

func pool(dataset string) string {
	name, _, _ := strings.Cut(dataset, "/")
	return name
}

// under reports whether path is dir or lies below it.
func under(path, dir string) bool {
	if path == dir || dir == "/" {
		return true
	}
	return strings.HasPrefix(path, dir+"/")
}

// DevicePrefix is where Linux lists volumes by name.
const DevicePrefix = "/dev/zvol/"

// Options says what one backup needs frozen.
type Options struct {
	// Agent is the id of this agent. It marks what the session creates.
	Agent string
	// Tag names the snapshots: dataset@Tag. Use something unique to the
	// backup job.
	Tag string
	// Paths are the folders and files to back up. Every dataset that
	// holds one, or is mounted below one, gets a snapshot.
	Paths []string
	// Volumes are the block devices to back up as images. Those that are
	// ZFS volumes, given as /dev/zvol/pool/name or pool/name, get a
	// snapshot. Others are left alone.
	Volumes []string
	// Live lists nothing in a snapshot: Paths are only searched for the
	// .zfs folders to leave out.
	Live bool
}

// Session is a set of snapshots held for one backup.
type Session struct {
	tag    string
	agent  string
	mounts []string          // mount points whose dataset has a usable snapshot, longest first
	vols   map[string]volume // by the name the policy used
	skip   []string          // .zfs folders under the paths
	snaps  []string          // dataset@tag, in creation order
	clones []string
	failed map[string]bool // volumes that are ZFS volumes but could not be frozen
}

type volume struct{ name, device string }

// Begin takes the snapshots. Whatever could not be frozen is described
// in warnings and is then read live by the caller; an error means
// nothing was done at all. Close the session when the backup has read
// everything.
func Begin(ctx context.Context, o Options) (s *Session, warnings []string, err error) {
	s = &Session{tag: o.Tag, agent: o.Agent, vols: map[string]volume{}, failed: map[string]bool{}}
	if !Available() {
		return s, nil, nil
	}
	all, err := List(ctx)
	if err != nil {
		return s, nil, err
	}
	if len(all) == 0 {
		return s, nil, nil
	}
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	// Which datasets matter: the one a path lives on and those mounted
	// below it.
	want := map[string]Dataset{}
	for _, p := range o.Paths {
		p, aerr := filepath.Abs(p)
		if aerr != nil {
			continue
		}
		for _, d := range all {
			if d.Volume || d.Mountpoint == "" {
				continue
			}
			if under(d.Mountpoint, p) {
				// A visible .zfs folder holds every snapshot of the
				// dataset; walking into it would back them all up.
				s.skip = append(s.skip, filepath.Join(d.Mountpoint, ".zfs"))
			}
			if o.Live {
				continue
			}
			if under(d.Mountpoint, p) {
				want[d.Name] = d
			} else if under(p, d.Mountpoint) {
				// Several datasets can be mounted above a path; the
				// one that holds it is on the same file system.
				if same, known := sameFS(p, d.Mountpoint); same || !known {
					want[d.Name] = d
				}
			}
		}
	}
	byName := map[string]Dataset{}
	for _, d := range all {
		byName[d.Name] = d
	}
	wantVol := map[string]string{} // dataset -> the policy's name for it
	for _, v := range o.Volumes {
		name := strings.TrimPrefix(v, DevicePrefix)
		if d, ok := byName[name]; ok && d.Volume {
			wantVol[d.Name] = v
		}
	}
	if len(want) == 0 && len(wantVol) == 0 {
		return s, nil, nil
	}

	// Remove what an earlier backup left behind when it was cut short.
	pools := map[string][]string{}
	for name := range want {
		pools[pool(name)] = append(pools[pool(name)], name)
	}
	for name := range wantVol {
		pools[pool(name)] = append(pools[pool(name)], name)
	}
	for p := range pools {
		for _, w := range cleanup(ctx, o.Agent, p) {
			warn("%s", w)
		}
	}

	// One command per pool: its snapshots are then taken at the same
	// instant, so files and volumes of a pool match each other.
	for p, names := range pools {
		sort.Strings(names)
		args := []string{"snapshot", "-o", Property + "=" + o.Agent}
		for _, n := range names {
			args = append(args, n+"@"+o.Tag)
		}
		if _, serr := run(ctx, args...); serr != nil {
			warn("could not take ZFS snapshots on pool %s, reading its live files: %v", p, serr)
			for _, n := range names {
				delete(want, n)
				if v, ok := wantVol[n]; ok {
					s.failed[v] = true
					delete(wantVol, n)
				}
			}
			continue
		}
		for _, n := range names {
			s.snaps = append(s.snaps, n+"@"+o.Tag)
		}
	}

	// A dataset's snapshot is read through its .zfs folder. One that is
	// not mounted where its property says (locked, or never mounted) has
	// no such folder, and its files are not on this machine to read.
	for _, d := range want {
		if fi, serr := os.Stat(filepath.Join(d.Mountpoint, ".zfs", "snapshot", o.Tag)); serr == nil && fi.IsDir() {
			s.mounts = append(s.mounts, d.Mountpoint)
		}
	}
	sort.Slice(s.mounts, func(i, j int) bool { return len(s.mounts[i]) > len(s.mounts[j]) })

	// A volume's snapshot has no device of its own, so it is read
	// through a clone: a second volume that shares the snapshot's blocks.
	for name, v := range wantVol {
		clone := name + "-" + o.Tag
		// Listed before it is known to exist: a clone command that
		// fails half way can still leave the clone behind, and the
		// snapshot cannot be removed while it is there.
		s.clones = append(s.clones, clone)
		// The clone is not made read-only. Setting that property in the
		// clone command makes ZFS mark a disk read-only that it has not
		// created yet, which crashed the kernel now and then (OpenZFS
		// 2.4.1, a NULL pointer in zvol_os_set_disk_ro). Nothing writes
		// to the clone anyway: it is opened for reading and destroyed.
		if _, cerr := run(ctx, "clone", "-o", Property+"="+o.Agent, "-o", "volmode=dev", name+"@"+o.Tag, clone); cerr != nil {
			warn("volume %s was not backed up: %v", v, cerr)
			s.failed[v] = true
			continue
		}
		dev := DevicePrefix + clone
		if !waitFor(ctx, dev, deviceWait) {
			warn("volume %s was not backed up: the device of its snapshot, %s, did not appear", v, dev)
			s.failed[v] = true
			continue
		}
		s.vols[v] = volume{name: DevicePrefix + name, device: dev}
	}
	return s, warnings, nil
}

func waitFor(ctx context.Context, path string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Snapshots lists what the session holds, as dataset@tag.
func (s *Session) Snapshots() []string { return s.snaps }

// Path turns a path into where to read it from: inside the snapshot of
// the dataset that holds it, or the path itself when there is none.
func (s *Session) Path(p string) string {
	for _, m := range s.mounts {
		if p == m {
			// The trailing slash matters. A snapshot is mounted on its
			// folder under .zfs, and asking about that folder without
			// following it (lstat, listing its attributes) answers
			// for the empty folder underneath instead of the
			// dataset's own top folder.
			return filepath.Join(m, ".zfs", "snapshot", s.tag) + "/"
		}
		if under(p, m) {
			return filepath.Join(m, ".zfs", "snapshot", s.tag, strings.TrimPrefix(p, m))
		}
	}
	return p
}

// Volume says how to read one of the volumes of Options. name is what to
// record in the backup, the volume's device path, and device is the
// frozen copy to read. zvol is false for anything that is not a ZFS
// volume; ok is false for a ZFS volume that could not be frozen, which
// must then not be backed up at all.
func (s *Session) Volume(v string) (name, device string, zvol, ok bool) {
	if s.failed[v] {
		return "", "", true, false
	}
	if vol, found := s.vols[v]; found {
		return vol.name, vol.device, true, true
	}
	return v, v, false, true
}

// Excludes lists the .zfs folders below the backed-up paths.
func (s *Session) Excludes() []string { return s.skip }

// Close removes the session's clones and snapshots.
func (s *Session) Close() error {
	// The backup's own context may be cancelled; cleaning up must still
	// happen.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var errs []string
	for _, c := range s.clones {
		if err := destroy(ctx, c); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, snap := range s.snaps {
		if err := destroy(ctx, snap); err != nil {
			errs = append(errs, err.Error())
		}
	}
	s.clones, s.snaps, s.mounts, s.vols = nil, nil, nil, map[string]volume{}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// destroy removes one snapshot or clone. A snapshot that was just read
// can still be mounted and busy for a moment.
func destroy(ctx context.Context, name string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if _, err = run(ctx, "destroy", name); err == nil || strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "could not find any snapshots") {
			return nil
		}
	}
	return err
}

// cleanup destroys the clones and snapshots in a pool that carry this
// agent's mark. It returns a message for each one it could not remove.
func cleanup(ctx context.Context, agent, pool string) (warnings []string) {
	if agent == "" {
		return nil
	}
	out, err := run(ctx, "get", "-H", "-r", "-p", "-s", "local", "-t", "volume,snapshot", "-o", "name,value", Property, pool)
	if err != nil {
		return nil
	}
	var clones, snaps []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 2 || f[1] != agent {
			continue
		}
		if strings.Contains(f[0], "@") {
			snaps = append(snaps, f[0])
		} else {
			clones = append(clones, f[0])
		}
	}
	for _, name := range append(clones, snaps...) {
		if err := destroy(ctx, name); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not remove %s, left over from an earlier backup: %v", name, err))
		}
	}
	return warnings
}
