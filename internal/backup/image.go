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
	// Close, if set, is called when the image has been read.
	Close func()
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
	for off := int64(0); off < src.Size; off += ImageBlock {
		if err := a.ctx.Err(); err != nil {
			return nil, err
		}
		n := min(int64(ImageBlock), src.Size-off)
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
		if src.SkipZero && allZero(buf[:n]) {
			ids = append(ids, keys.ID{})
			a.progress()
			continue
		}
		id, err := a.put(buf[:n])
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		a.progress()
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
