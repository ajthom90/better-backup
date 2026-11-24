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

func (driver) List(context.Context) ([]string, error) { return List() }

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
	dir := filepath.Join(t.Place, hypervisor.SafeName(t.Name))
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

func (r *restore) Disk(name string, size int64) (io.WriterAt, bool, func() error, error) {
	return r.t.OpenFile(filepath.Join(r.dir, name), size)
}

func (r *restore) Finish() ([]string, error) { return Create(r.cfg, r.dir, r.t.Name) }
