package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/qemuimg"
)

type restore struct {
	ctx     context.Context
	cfg     Config
	id      string // the new machine's id
	name    string
	storage string            // where the disks go; "" for where each was
	volumes map[string]string // drive key -> new volume
	kinds   map[string]string // storage -> its type
}

var created = regexp.MustCompile(`'([^']+:[^']+)'`)

// NewRestore creates an empty machine to restore into. t.Place names
// the storage for its disks; empty puts each disk on the storage it
// was on. The machine gets its old id if that is free, which it is
// when the original is gone, and the next free id otherwise.
func (d driver) NewRestore(ctx context.Context, config []byte, t hypervisor.Target) (hypervisor.Restore, error) {
	var cfg Config
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("reading its configuration: %w", err)
	}
	if cfg.Conf == "" {
		return nil, errors.New("its configuration in this snapshot is incomplete")
	}
	if err := d.Available(); err != nil {
		return nil, err
	}
	if strings.ContainsAny(t.Place, `/\ `) {
		return nil, fmt.Errorf("on Proxmox the target is the name of a storage, such as local-lvm, not a folder (%q); leave it empty to use the storages the disks were on", t.Place)
	}
	r := &restore{ctx: ctx, cfg: cfg, name: t.Name, storage: t.Place, volumes: map[string]string{}, kinds: map[string]string{}}
	// "Name Type Status ..." per storage.
	status, err := run(ctx, "pvesm", "status")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(status, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] != "Name" {
			r.kinds[f[0]] = f[1]
		}
	}
	for _, dk := range cfg.Disks {
		if st := r.place(dk); r.kinds[st] == "" {
			return nil, fmt.Errorf("this node has no storage named %q for the disk %s; name a storage as the target", st, dk.Key)
		}
	}
	if cfg.VMID > 0 {
		if _, err := run(ctx, "pvesh", "get", "/cluster/nextid", "--vmid", strconv.Itoa(cfg.VMID)); err == nil {
			r.id = strconv.Itoa(cfg.VMID)
		}
	}
	if r.id == "" {
		out, err := run(ctx, "pvesh", "get", "/cluster/nextid")
		if err != nil {
			return nil, fmt.Errorf("asking for a free machine id: %w", err)
		}
		r.id = strings.Trim(out, "\" \n")
		if _, err := strconv.Atoi(r.id); err != nil {
			return nil, fmt.Errorf("asking for a free machine id: unexpected answer %q", out)
		}
	}
	if _, err := run(ctx, "qm", "create", r.id, "--name", t.Name); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *restore) place(dk Disk) string {
	if r.storage != "" {
		return r.storage
	}
	return dk.Storage
}

func (r *restore) Where() string {
	if r.storage != "" {
		return fmt.Sprintf("the storage %s, for the machine with id %s", r.storage, r.id)
	}
	return "their original storages, for the machine with id " + r.id
}

func (r *restore) Disk(name string, size int64) (io.WriterAt, bool, func() error, error) {
	var dk *Disk
	for i := range r.cfg.Disks {
		if r.cfg.Disks[i].File == name {
			dk = &r.cfg.Disks[i]
		}
	}
	if dk == nil {
		return nil, false, nil, errors.New("the machine's configuration does not mention it")
	}
	storage := r.place(*dk)
	kb := strconv.FormatInt((size+1023)/1024, 10)
	// Keep the disk's format where the storage has a choice. A storage
	// without one refuses the option, and then picks its own.
	out, err := run(r.ctx, "pvesm", "alloc", storage, r.id, "", kb, "--format", dk.Format)
	if err != nil {
		var err2 error
		if out, err2 = run(r.ctx, "pvesm", "alloc", storage, r.id, "", kb); err2 != nil {
			return nil, false, nil, err
		}
	}
	m := created.FindStringSubmatch(out)
	if m == nil {
		return nil, false, nil, fmt.Errorf("pvesm alloc: unexpected answer %q", out)
	}
	volume := m[1]
	path, _, err := volumePath(r.ctx, volume)
	if err != nil {
		return nil, false, nil, err
	}
	im, err := qemuimg.Open(r.ctx, path, volumeFormat(volume), true)
	if err != nil {
		return nil, false, nil, err
	}
	if im.Size() < size {
		im.Close()
		return nil, false, nil, fmt.Errorf("the new volume %s holds %d bytes and the disk needs %d", volume, im.Size(), size)
	}
	r.volumes[dk.Key] = volume
	// A new volume reads as zeros, except on plain LVM, which hands out
	// whatever the disk held before.
	fresh := r.kinds[storage] != "lvm"
	return im, fresh, im.Close, nil
}

func (r *restore) Finish() ([]string, error) {
	first, rest, warnings := restoreSettings(parseConf(r.cfg.Conf), r.volumes, r.storage)
	set := func(ss []setting) error {
		args := []string{"set", r.id}
		for _, s := range ss {
			args = append(args, "--"+s.Key, s.Value)
		}
		_, err := run(r.ctx, "qm", args...)
		return err
	}
	// Disks first: other settings, such as the boot order, refer to
	// them. A disk that cannot be attached fails the restore.
	for _, s := range first {
		if err := set([]setting{s}); err != nil {
			if _, isDisk := r.volumes[s.Key]; isDisk {
				return warnings, err
			}
			warnings = append(warnings, fmt.Sprintf("its drive %s could not be put back: %v", s.Key, err))
		}
	}
	// Then everything else at once, or one by one if Proxmox refuses
	// the lot, so that one setting it no longer accepts does not cost
	// the others.
	if len(rest) > 0 && set(rest) != nil {
		for _, s := range rest {
			if err := set([]setting{s}); err != nil {
				warnings = append(warnings, fmt.Sprintf("its setting %q could not be put back: %v", s.Key, err))
			}
		}
	}
	return warnings, nil
}
