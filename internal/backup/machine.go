package backup

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// MachineSource is a whole machine to back up: the layout of the disks
// it starts from and an image of every partition worth keeping. It is
// what a bare-metal restore needs.
type MachineSource struct {
	Layout disklayout.Machine
	// Images holds one image per partition whose Image field is set in
	// the layout, under that name.
	Images []ImageSource
	// Close, if set, is called when the machine has been read.
	Close func()
}

// MachinePlan is a machine whose disks have been looked at but not yet
// read. Between making the plan and opening it the caller takes the
// volume snapshots the plan asks for.
type MachinePlan struct {
	Layout disklayout.Machine
	// SnapPaths are the volumes that must be read from a snapshot
	// (Windows): images of mounted NTFS volumes are only usable when they
	// are taken from one.
	SnapPaths []string
	// Warnings are things the administrator should know about.
	Warnings []string

	parts  []planPart
	closer []io.Closer
}

// planPart says how one partition is read.
type planPart struct {
	name string
	size int64
	// volume is the mounted volume to read through a snapshot, such as
	// `C:\`. Empty: read raw.
	volume string
	raw    io.ReaderAt
	// used limits what is kept of a partition whose contents are not
	// worth more, such as swap space.
	used []Extent
}

// Close releases the disks of a plan that was not opened.
func (p *MachinePlan) Close() {
	for _, c := range p.closer {
		c.Close()
	}
	p.closer = nil
}

// Open turns the plan into something to back up. mapPath maps a volume
// such as `C:\` to its snapshot device; a volume that maps to itself has
// no snapshot, and then the machine cannot be backed up.
func (p *MachinePlan) Open(mapPath func(string) string) (*MachineSource, error) {
	src := &MachineSource{Layout: p.Layout, Close: p.Close}
	fail := func(err error) (*MachineSource, error) {
		for _, im := range src.Images {
			if im.Close != nil {
				im.Close()
			}
		}
		p.Close()
		return nil, err
	}
	for _, part := range p.parts {
		if part.volume != "" {
			im, err := openSnapshotVolume(part.name, part.volume, mapPath)
			if err != nil {
				return fail(err)
			}
			if im.Size > part.size {
				im.Close()
				return fail(fmt.Errorf("volume %s is larger than its partition", part.volume))
			}
			src.Images = append(src.Images, im)
			continue
		}
		src.Images = append(src.Images, ImageSource{Name: part.name, Size: part.size, R: part.raw, Used: part.used, SkipZero: part.used == nil})
	}
	return src, nil
}

// machine stores a whole machine. If any partition cannot be read the
// machine is left out: a machine that would not start is not a backup.
func (a *archiver) machine(src *MachineSource, parent *tree.Entry) (*tree.Entry, error) {
	if src.Close != nil {
		defer src.Close()
	}
	closeFrom := func(i int) {
		for ; i < len(src.Images); i++ {
			if c := src.Images[i].Close; c != nil {
				c()
			}
		}
	}
	prev := map[string]*tree.Entry{}
	if parent != nil && parent.Type == tree.TypeMachine {
		for _, p := range parent.Subtree {
			es, err := tree.ReadPage(a.get, p)
			if err != nil {
				prev = map[string]*tree.Entry{}
				break
			}
			for i := range es {
				prev[es[i].Name] = &es[i]
			}
		}
	}
	now := time.Now().UnixNano()
	layout, err := json.Marshal(src.Layout)
	if err != nil {
		closeFrom(0)
		return nil, err
	}
	pw := tree.NewPageWriter(a.put)
	id, err := a.put(layout)
	if err != nil {
		closeFrom(0)
		return nil, err
	}
	if err := pw.Add(&tree.Entry{Name: disklayout.FileName, Type: tree.TypeFile, Mode: 0o600, Size: int64(len(layout)), ModTime: now, Content: []keys.ID{id}}); err != nil {
		closeFrom(0)
		return nil, err
	}
	for i := range src.Images {
		e, err := a.image(&src.Images[i], prev[src.Images[i].Name])
		if err != nil {
			closeFrom(i + 1)
			return nil, err
		}
		if e == nil {
			closeFrom(i + 1)
			a.warn("the whole machine was not backed up because %s could not be read", src.Images[i].Name)
			return nil, nil
		}
		a.stats.Files-- // the machine is counted once, below
		if err := pw.Add(e); err != nil {
			closeFrom(i + 1)
			return nil, err
		}
	}
	pages, err := pw.Finish()
	if err != nil {
		return nil, err
	}
	a.stats.Files++
	a.stats.FilesChanged++
	return &tree.Entry{Name: tree.MachineName, Type: tree.TypeMachine, ModTime: now, Subtree: pages}, nil
}

// planDisk reads one disk's partition table and decides how each of its
// partitions is read. raw reads the whole disk. part, if set, may give a
// better reader for one partition (its own device), or name the mounted
// volume to read through a snapshot instead.
func (p *MachinePlan) planDisk(d *disklayout.Disk, raw io.ReaderAt, part func(*disklayout.Partition) (r io.ReaderAt, volume string)) {
	const mib = 1 << 20
	for i := range d.Partitions {
		pt := &d.Partitions[i]
		if pt.Kind == disklayout.KindExtended || pt.Kind == disklayout.KindMSR {
			continue // a container, or space that holds nothing
		}
		var r io.ReaderAt = io.NewSectionReader(raw, pt.Offset, pt.Size)
		volume := ""
		if part != nil {
			var pr io.ReaderAt
			if pr, volume = part(pt); pr != nil {
				r = pr
			}
		}
		if pt.FS == "" {
			pt.FS, pt.Label = disklayout.Sniff(r)
		}
		pt.Image = fmt.Sprintf("%s-part%d", d.Name, pt.Number)
		pp := planPart{name: pt.Image, size: pt.Size, raw: r, volume: volume}
		if pt.Kind == disklayout.KindSwap || pt.FS == "swap" {
			// Only the header matters: it carries the identifier the
			// system finds its swap space by.
			pp.used = []Extent{{0, min(pt.Size, mib)}}
		}
		p.parts = append(p.parts, pp)
	}
}
