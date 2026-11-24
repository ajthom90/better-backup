package backup

import (
	"io"
	"time"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// ImageBlock is the block size of image backups. Blocks sit at fixed
// offsets, so a change in one place never shifts the blocks after it and
// unchanged blocks deduplicate against earlier backups.
const ImageBlock = 1 << 20

// Extent is a byte range.
type Extent struct{ Off, Len int64 }

// ImageSource is a volume or disk to back up block by block.
type ImageSource struct {
	// Name is recorded in the snapshot, such as `C:` or /dev/vg0/data.
	Name string
	Size int64
	R    io.ReaderAt
	// Used lists the allocated ranges in ascending order. Blocks outside
	// them are not read or stored. Nil means everything is in use.
	Used []Extent
	// SkipZero records blocks of zeros as holes instead of storing them.
	// It suits virtual disks, where unwritten parts read as zeros.
	SkipZero bool
	// Tracked says Changed is valid: it lists, in ascending order, every
	// part written since the previous backup of this image. Blocks
	// outside it are taken from that backup without being read. The
	// caller must be sure the list is complete; a missing range means a
	// backup that silently holds old data.
	Tracked bool
	Changed []Extent
	// Verify reads the blocks Changed leaves out all the same and warns
	// if any differs from the previous backup. The backup is then right
	// whatever the list says. It is the way to test change tracking.
	Verify bool
	// Close, if set, is called when the image has been read.
	Close func()
}

// previous returns the block ids of the last backup of an image if they
// can stand in for blocks of src.
func (a *archiver) previous(src *ImageSource, parent *tree.Entry) []keys.ID {
	if !src.Tracked || parent == nil || parent.Type != tree.TypeImage || parent.Size != src.Size ||
		parent.BlockSize != ImageBlock || parent.HolesZero != src.SkipZero {
		return nil
	}
	var ids []keys.ID
	if err := tree.EachContent(a.get, parent, func(id keys.ID) error { ids = append(ids, id); return nil }); err != nil {
		return nil
	}
	if int64(len(ids)) != (src.Size+ImageBlock-1)/ImageBlock {
		return nil
	}
	return ids
}

// image stores one image. A source that cannot be read is skipped with a
// warning; only store errors are returned.
func (a *archiver) image(src *ImageSource, parent *tree.Entry) (*tree.Entry, error) {
	if src.Close != nil {
		defer src.Close()
	}
	e := &tree.Entry{Name: src.Name, Type: tree.TypeImage, Size: src.Size, BlockSize: ImageBlock, HolesZero: src.SkipZero, ModTime: time.Now().UnixNano()}
	ids := make([]keys.ID, 0, (src.Size+ImageBlock-1)/ImageBlock)
	buf := make([]byte, ImageBlock)
	used := src.Used
	prev := a.previous(src, parent)
	changed := src.Changed
	var kept, wrong int64
	for off := int64(0); off < src.Size; off += ImageBlock {
		if err := a.ctx.Err(); err != nil {
			return nil, err
		}
		n := min(int64(ImageBlock), src.Size-off)
		same := false
		if prev != nil {
			for len(changed) > 0 && changed[0].Off+changed[0].Len <= off {
				changed = changed[1:]
			}
			same = len(changed) == 0 || changed[0].Off >= off+n
			if same && !src.Verify {
				ids = append(ids, prev[off/ImageBlock])
				kept++
				continue
			}
		}
		if src.Used != nil {
			for len(used) > 0 && used[0].Off+used[0].Len <= off {
				used = used[1:]
			}
			if len(used) == 0 || used[0].Off >= off+n {
				ids = append(ids, keys.ID{})
				continue
			}
		}
		if got, err := src.R.ReadAt(buf[:n], off); int64(got) != n {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			a.warn("%s: reading at offset %d: %v; this image was not backed up", src.Name, off, err)
			return nil, nil
		}
		a.stats.BytesRead += n
		var id keys.ID
		if !src.SkipZero || !allZero(buf[:n]) {
			var err error
			if id, err = a.put(buf[:n]); err != nil {
				return nil, err
			}
		}
		if same {
			kept++
			if id != prev[off/ImageBlock] {
				wrong++
			}
		}
		ids = append(ids, id)
		a.progress()
	}
	if wrong > 0 {
		a.warn("%s: change tracking is not reliable here: %d of the %d blocks it reported as unchanged had changed. This backup is correct because every block was read; do not switch change tracking on for this machine", src.Name, wrong, kept)
	}
	if err := tree.SetContent(e, ids, a.put); err != nil {
		return nil, err
	}
	a.stats.Files++
	a.stats.Bytes += src.Size
	if parent == nil || parent.Type != tree.TypeImage {
		a.stats.FilesChanged++
	}
	return e, nil
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if b[0]|b[1]|b[2]|b[3]|b[4]|b[5]|b[6]|b[7] != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// VMSource is a virtual machine to back up: its configuration and the
// frozen disks to image.
type VMSource struct {
	Name string
	// ConfigName and Config become a small file in the snapshot.
	ConfigName string
	Config     []byte
	Disks      []ImageSource
	// Open, if set, is called just before the machine is read and fills
	// in the fields above. Taking the checkpoint here, and not when the
	// job starts, keeps each machine's checkpoint short-lived.
	// prev is the Config of the machine's previous backup, or nil.
	Open func(vm *VMSource, prev []byte) error
	// Check, if set, is called after the disks were read and before
	// Close. An error means what was read cannot be trusted, such as
	// when the hypervisor's frozen view of the disks broke half way;
	// the machine is then left out of the backup.
	Check func() error
	// Close, if set, is called when the machine has been read. It is
	// where a checkpoint is released.
	Close func()
}

// vm stores one virtual machine. If any disk cannot be read the whole
// machine is left out: half a machine is not a backup.
func (a *archiver) vm(src *VMSource, parent *tree.Entry) (*tree.Entry, error) {
	// The previous backup's configuration and disks, for change tracking.
	prev := map[string]*tree.Entry{}
	var prevConfig []byte
	if parent != nil && parent.Type == tree.TypeVM {
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
	if src.Open != nil {
		for _, e := range prev {
			if e.Type == tree.TypeFile && e.Size < 1<<20 && len(e.Content) == 1 && prevConfig == nil {
				prevConfig, _ = a.get(e.Content[0])
			}
		}
		if err := src.Open(src, prevConfig); err != nil {
			a.warn("virtual machine %s was not backed up: %v", src.Name, err)
			return nil, nil
		}
	}
	if src.Close != nil {
		defer src.Close()
	}
	now := time.Now().UnixNano()
	pw := tree.NewPageWriter(a.put)
	id, err := a.put(src.Config)
	if err != nil {
		return nil, err
	}
	cfg := &tree.Entry{Name: src.ConfigName, Type: tree.TypeFile, Mode: 0o600, Size: int64(len(src.Config)), ModTime: now, Content: []keys.ID{id}}
	if err := pw.Add(cfg); err != nil {
		return nil, err
	}
	failed := false
	for i := range src.Disks {
		if failed {
			if c := src.Disks[i].Close; c != nil {
				c()
			}
			continue
		}
		e, err := a.image(&src.Disks[i], prev[src.Disks[i].Name])
		if err != nil {
			return nil, err
		}
		if e == nil {
			failed = true
			continue
		}
		a.stats.Files-- // the machine is counted once, below
		if err := pw.Add(e); err != nil {
			return nil, err
		}
	}
	if failed {
		a.warn("virtual machine %s was not backed up because one of its disks could not be read", src.Name)
		return nil, nil
	}
	if src.Check != nil {
		if err := src.Check(); err != nil {
			a.warn("virtual machine %s was not backed up: %v", src.Name, err)
			return nil, nil
		}
	}
	pages, err := pw.Finish()
	if err != nil {
		return nil, err
	}
	a.stats.Files++
	a.stats.FilesChanged++
	return &tree.Entry{Name: src.Name, Type: tree.TypeVM, ModTime: now, Subtree: pages}, nil
}
