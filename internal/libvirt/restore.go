package libvirt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/qemuimg"
)

type restore struct {
	ctx     context.Context
	cfg     Config
	dir     string
	name    string
	paths   map[string]string // disk target -> restored file
	replace bool              // the machine of this name was set aside
}

// NewRestore prepares to build a machine from a backup. Its disks are
// written as qcow2 files into a folder named after the machine under
// t.Place, whatever they were before: files in another format, or
// block devices.
func (d driver) NewRestore(ctx context.Context, config []byte, t hypervisor.Target) (hypervisor.Restore, error) {
	var cfg Config
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("reading its configuration: %w", err)
	}
	if cfg.XML == "" {
		return nil, errors.New("its configuration in this snapshot is incomplete")
	}
	if err := d.Available(); err != nil {
		return nil, err
	}
	if t.Place == "" {
		return nil, errors.New("it needs a target folder on the KVM host for its disks, such as /var/lib/libvirt/images")
	}
	if strings.ContainsAny(t.Name, "/\n") || t.Name == "" {
		return nil, fmt.Errorf("%q cannot be the name of a machine", t.Name)
	}
	if _, err := virsh(ctx, "domstate", t.Name); err == nil && !t.Replace {
		return nil, fmt.Errorf("a virtual machine named %q already exists; choose another name", t.Name)
	}
	// Check now that the definition can be rewritten, not after hours
	// of restoring disks.
	if _, err := rewrite([]byte(cfg.XML), t.Name, false, nil, ""); err != nil {
		return nil, err
	}
	dir := filepath.Join(t.Place, t.FolderFor())
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return nil, err
	}
	return &restore{ctx: ctx, cfg: cfg, dir: dir, name: t.Name, paths: map[string]string{}, replace: t.Replace}, nil
}

func (r *restore) Where() string { return r.dir }

func (r *restore) Disk(spec hypervisor.DiskSpec) (io.WriterAt, bool, func() error, error) {
	name, size := spec.Name, spec.Size
	var dk *Disk
	for i := range r.cfg.Disks {
		if r.cfg.Disks[i].File == name {
			dk = &r.cfg.Disks[i]
		}
	}
	if dk == nil {
		return nil, false, nil, errors.New("the machine's configuration does not mention it")
	}
	path := filepath.Join(r.dir, hypervisor.SafeName(dk.Target)+".qcow2")
	if _, err := os.Lstat(path); err == nil {
		return nil, false, nil, fmt.Errorf("%s already exists; choose another folder or name", path)
	}
	if err := qemuimg.Create(r.ctx, path, "qcow2", size); err != nil {
		return nil, false, nil, err
	}
	im, err := qemuimg.Open(r.ctx, path, "qcow2", true)
	if err != nil {
		return nil, false, nil, err
	}
	r.paths[dk.Target] = path
	return im, true, im.Close, nil
}

func (r *restore) Finish() ([]string, error) {
	var warnings []string
	nvram := ""
	if len(r.cfg.NVRAM) > 0 {
		raw, err := gunzip(r.cfg.NVRAM)
		if err == nil {
			nvram = filepath.Join(r.dir, hypervisor.SafeName(r.name)+"_VARS.fd")
			err = os.WriteFile(nvram, raw, 0o600)
		}
		if err != nil {
			nvram = ""
			warnings = append(warnings, fmt.Sprintf("its UEFI settings could not be restored, so the firmware starts with its defaults: %v", err))
		}
	}
	// Keep the machine's identity if the original is gone; beside the
	// original it has to get a new one.
	keep := false
	if r.cfg.UUID != "" {
		_, err := virsh(r.ctx, "domname", r.cfg.UUID)
		keep = err != nil
	}
	doc, err := rewrite([]byte(r.cfg.XML), r.name, keep, r.paths, nvram)
	if err != nil {
		return warnings, err
	}
	f, err := os.CreateTemp("", "bb-domain-*.xml")
	if err != nil {
		return warnings, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(doc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return warnings, err
	}
	if _, err := virsh(r.ctx, "define", f.Name()); err != nil {
		return warnings, err
	}
	if r.cfg.UUID != "" && !keep && !r.replace && strings.Contains(r.cfg.XML, "<mac ") {
		warnings = append(warnings, "the original machine still exists on this host, and the restored one has the same network (MAC) addresses; do not run both on the same network")
	}
	for _, d := range r.cfg.Disks {
		if _, ok := r.paths[d.Target]; !ok {
			warnings = append(warnings, fmt.Sprintf("its disk %s was not in the backup and still points at %s", d.Target, d.Path))
		}
	}
	return warnings, nil
}

// Find, SetAside and PutBack make driver a hypervisor.Replacer.

func (driver) Find(ctx context.Context, _ *hypervisor.Connection, name string) (string, bool, error) {
	if _, err := virsh(ctx, "domstate", name); err != nil {
		return "", false, nil
	}
	// The files of the first disk that is a file say where: its folder.
	out, err := virsh(ctx, "domblklist", name, "--details")
	if err != nil {
		return "", true, err
	}
	for _, line := range lines(out) {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "file" && f[1] == "disk" && strings.HasPrefix(f[3], "/") {
			return filepath.Dir(strings.Join(f[3:], " ")), true, nil
		}
	}
	return "", true, nil
}

func (driver) SetAside(ctx context.Context, _ *hypervisor.Connection, name, aside string) error {
	if st, _ := virsh(ctx, "domstate", name); !strings.HasPrefix(strings.TrimSpace(st), "shut off") {
		// Ask it to shut down, and pull the plug if it does not.
		virsh(ctx, "shutdown", name)
		for i := 0; i < 60; i++ {
			if st, _ := virsh(ctx, "domstate", name); strings.HasPrefix(strings.TrimSpace(st), "shut off") {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		if st, _ := virsh(ctx, "domstate", name); !strings.HasPrefix(strings.TrimSpace(st), "shut off") {
			if _, err := virsh(ctx, "destroy", name); err != nil {
				return fmt.Errorf("switching it off: %w", err)
			}
		}
	}
	// libvirt does not rename a machine that has checkpoints. The ones
	// change tracking made are of no use to a machine that is no longer
	// backed up, so libvirt is told to forget them, newest first.
	if out, err := virsh(ctx, "checkpoint-list", name, "--name", "--topological"); err == nil {
		cps := lines(out)
		for i := len(cps) - 1; i >= 0; i-- {
			if strings.HasPrefix(cps[i], "bb-") {
				virsh(ctx, "checkpoint-delete", name, cps[i], "--metadata")
			}
		}
	}
	if _, err := virsh(ctx, "domrename", name, aside); err != nil {
		return fmt.Errorf("%w (libvirt does not rename a machine that has snapshots or checkpoints of its own; remove them first, or restore under a new name instead)", err)
	}
	return nil
}

func (driver) PutBack(ctx context.Context, _ *hypervisor.Connection, name, aside string) error {
	_, err := virsh(ctx, "domrename", aside, name)
	return err
}
