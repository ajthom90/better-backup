package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/hypervisor"
)

func init() { hypervisor.Register(driver{}) }

// driver makes Hyper-V one of the hypervisors of package hypervisor.
type driver struct{}

func (driver) Kind() string       { return "hyperv" }
func (driver) Label() string      { return "Hyper-V" }
func (driver) ConfigFile() string { return ConfigFile }

func (driver) Available() error {
	if runtime.GOOS != "windows" {
		return ErrUnsupported
	}
	return nil
}

func (driver) List(context.Context, *hypervisor.Connection) ([]string, error) { return List() }

func (driver) Freeze(_ context.Context, vm string, o hypervisor.Options) (*hypervisor.Frozen, error) {
	ho := Options{Tracking: o.Tracking, StateDir: o.StateDir}
	if p := new(Config); len(o.Previous) > 0 && json.Unmarshal(o.Previous, p) == nil {
		ho.Previous = p
	}
	cp, err := Prepare(vm, "job "+o.Tag, ho)
	if err != nil {
		return nil, err
	}
	release := func() error {
		if err := cp.Release(); err != nil {
			return fmt.Errorf("removing the backup checkpoint failed; remove it in Hyper-V Manager: %w", err)
		}
		return nil
	}
	f := &hypervisor.Frozen{Warnings: cp.Warnings, Release: release}
	if f.Config, err = json.Marshal(cp.Config); err != nil {
		return nil, errors.Join(err, release())
	}
	for _, d := range cp.Config.Disks {
		src, err := backup.OpenImage(d.File, d.Path)
		if err != nil {
			for _, o := range f.Disks {
				o.Close()
			}
			return nil, errors.Join(fmt.Errorf("opening its disk %s: %w", d.Path, err), release())
		}
		src.Tracked = d.Tracked
		for _, r := range d.Changed {
			src.Changed = append(src.Changed, backup.Extent{Off: r.Off, Len: r.Len})
		}
		f.Disks = append(f.Disks, src)
	}
	return f, nil
}

func (d driver) NewRestore(_ context.Context, config []byte, t hypervisor.Target) (hypervisor.Restore, error) {
	var cfg Config
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("reading its configuration: %w", err)
	}
	if cfg.Generation == 0 {
		return nil, errors.New("its configuration in this snapshot is incomplete")
	}
	if err := d.Available(); err != nil {
		return nil, err
	}
	if t.Place == "" {
		return nil, errors.New("it needs a target folder on the Hyper-V host for its files")
	}
	dir := filepath.Join(t.Place, t.FolderFor())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &restore{cfg: cfg, dir: dir, t: t}, nil
}

type restore struct {
	cfg Config
	dir string
	t   hypervisor.Target
}

func (r *restore) Where() string { return r.dir }

func (r *restore) Disk(d hypervisor.DiskSpec) (io.WriterAt, bool, func() error, error) {
	return r.t.OpenFile(filepath.Join(r.dir, d.Name), d.Size)
}

func (r *restore) Finish() ([]string, error) { return Create(r.cfg, r.dir, r.t.Name) }

// Find, SetAside and PutBack make driver a hypervisor.Replacer.

func (driver) Find(_ context.Context, _ *hypervisor.Connection, name string) (string, bool, error) {
	disk, found, err := Locate(name)
	if err != nil || !found {
		return "", found, err
	}
	return placeBeside(disk), true, nil
}

func (driver) SetAside(_ context.Context, _ *hypervisor.Connection, name, aside string) error {
	return Rename(name, aside, true)
}

func (driver) PutBack(_ context.Context, _ *hypervisor.Connection, name, aside string) error {
	return Rename(aside, name, false)
}

// placeBeside says where a copy of a machine goes whose first disk is
// the file disk: next to the folder that holds the disk, or in that
// folder if it is the top of a drive. An empty disk gives "".
func placeBeside(disk string) string {
	if disk == "" {
		return ""
	}
	dir := winDir(disk)
	if up := winDir(dir); up != dir && len(strings.TrimRight(up, `\/`)) > 2 {
		return up
	}
	return dir
}

// winDir is filepath.Dir for a Windows path, on any system.
func winDir(p string) string {
	p = strings.TrimRight(p, `\/`)
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return p
	}
	if i == 2 && p[1] == ':' {
		return p[:3]
	}
	return p[:i]
}
