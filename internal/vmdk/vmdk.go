// Package vmdk reads and writes VMware's stream-optimized disk format,
// the one vSphere hands out when a disk is exported and takes when one
// is imported. It is made to be read and written front to back: a
// header, then the disk's 64 KiB grains that hold data, each behind a
// small marker and compressed with deflate, in ascending order, with
// tables of where they are in between and at the end.
package vmdk

import (
	"bufio"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
)

const (
	sector     = 512
	magic      = 0x564d444b // "KDMV"
	grainSize  = 128        // sectors: 64 KiB
	grainBytes = grainSize * sector
	gtEntries  = 512 // grains per grain table
	gtSectors  = gtEntries * 4 / sector

	flagNewline    = 1 << 0
	flagCompressed = 1 << 16
	flagMarkers    = 1 << 17

	markerEOS    = 0
	markerGT     = 1
	markerGD     = 2
	markerFooter = 3

	gdAtEnd = 0xffffffffffffffff

	// What the writer puts before the first grain: the header, then
	// room for the descriptor.
	descSectors = 20
	overhead    = 128
)

// header is the 512-byte start (and footer) of a sparse extent.
type header struct {
	Capacity   uint64 // sectors
	GrainSize  uint64
	DescOffset uint64
	DescSize   uint64
	GDOffset   uint64
	Overhead   uint64
	Flags      uint32
	Compress   uint16
}

func (h *header) encode() []byte {
	b := make([]byte, sector)
	le := binary.LittleEndian
	le.PutUint32(b[0:], magic)
	le.PutUint32(b[4:], 3) // version
	le.PutUint32(b[8:], h.Flags)
	le.PutUint64(b[12:], h.Capacity)
	le.PutUint64(b[20:], h.GrainSize)
	le.PutUint64(b[28:], h.DescOffset)
	le.PutUint64(b[36:], h.DescSize)
	le.PutUint32(b[44:], gtEntries)
	le.PutUint64(b[48:], 0) // no redundant grain directory
	le.PutUint64(b[56:], h.GDOffset)
	le.PutUint64(b[64:], h.Overhead)
	b[72] = 0 // clean
	b[73], b[74], b[75], b[76] = '\n', ' ', '\r', '\n'
	le.PutUint16(b[77:], h.Compress)
	return b
}

func decodeHeader(b []byte) (*header, error) {
	le := binary.LittleEndian
	if len(b) < sector || le.Uint32(b[0:]) != magic {
		return nil, errors.New("not a VMware sparse disk")
	}
	h := &header{Flags: le.Uint32(b[8:]), Capacity: le.Uint64(b[12:]), GrainSize: le.Uint64(b[20:]), DescOffset: le.Uint64(b[28:]),
		DescSize: le.Uint64(b[36:]), GDOffset: le.Uint64(b[56:]), Overhead: le.Uint64(b[64:]), Compress: le.Uint16(b[77:])}
	if v := le.Uint32(b[4:]); v < 1 || v > 3 {
		return nil, fmt.Errorf("unknown VMDK version %d", v)
	}
	if h.Flags&flagCompressed == 0 || h.Flags&flagMarkers == 0 || h.Compress != 1 {
		return nil, errors.New("the VMDK is not stream-optimized (compressed, with markers)")
	}
	if h.GrainSize == 0 || h.GrainSize > 4096 || h.GrainSize&(h.GrainSize-1) != 0 {
		return nil, fmt.Errorf("unsupported grain size of %d sectors", h.GrainSize)
	}
	if h.Capacity > 1<<53 {
		return nil, errors.New("implausible disk size")
	}
	return h, nil
}

// Reader reads a stream-optimized disk from a stream. It gives the
// disk's contents through ReadAt, but only front to back: each call
// must start at or after the end of the one before.
type Reader struct {
	r     *bufio.Reader
	h     *header
	pos   int64 // sectors consumed from the stream
	next  int64 // end of the last ReadAt
	grain []byte
	at    int64 // byte offset of grain in the disk; -1: none buffered
	last  int64 // end of the last grain seen, to check their order
	done  bool
	buf   []byte
}

// NewReader reads the header of a stream-optimized disk.
func NewReader(r io.Reader) (*Reader, error) {
	d := &Reader{r: bufio.NewReaderSize(r, 1<<20), at: -1}
	b := make([]byte, sector)
	if _, err := io.ReadFull(d.r, b); err != nil {
		return nil, fmt.Errorf("reading the VMDK header: %w", err)
	}
	h, err := decodeHeader(b)
	if err != nil {
		return nil, err
	}
	d.h, d.pos = h, 1
	// Grains start after the overhead; the descriptor in between is of
	// no use here.
	if h.Overhead < 1 || h.Overhead > 1<<20 {
		return nil, errors.New("implausible VMDK header")
	}
	if err := d.skip(int64(h.Overhead) - 1); err != nil {
		return nil, err
	}
	return d, nil
}

// Size is the disk's size in bytes.
func (d *Reader) Size() int64 { return int64(d.h.Capacity) * sector }

func (d *Reader) skip(sectors int64) error {
	n, err := io.CopyN(io.Discard, d.r, sectors*sector)
	d.pos += n / sector
	if err != nil {
		return fmt.Errorf("the VMDK stream ended early: %w", err)
	}
	return nil
}

// advance reads up to the next grain and buffers it. At the end of
// the stream it sets done.
func (d *Reader) advance() error {
	var m [sector]byte
	for {
		if _, err := io.ReadFull(d.r, m[:]); err != nil {
			return fmt.Errorf("the VMDK stream ended early: %w", err)
		}
		d.pos++
		val, size := binary.LittleEndian.Uint64(m[0:]), binary.LittleEndian.Uint32(m[8:])
		if size == 0 {
			switch typ := binary.LittleEndian.Uint32(m[12:]); typ {
			case markerEOS:
				d.done = true
				return nil
			case markerGT, markerGD, markerFooter:
				if val > 1<<24 {
					return errors.New("implausible table in the VMDK stream")
				}
				if err := d.skip(int64(val)); err != nil {
					return err
				}
				continue
			default:
				return fmt.Errorf("unknown marker %d in the VMDK stream", typ)
			}
		}
		gb := int64(d.h.GrainSize) * sector
		if val >= d.h.Capacity || int64(size) > 2*gb+1024 {
			return errors.New("implausible grain in the VMDK stream")
		}
		// The data starts in the marker's own sector.
		comp := make([]byte, 0, size)
		comp = append(comp, m[12:min(sector, 12+int(size))]...)
		if rest := int(size) - len(comp); rest > 0 {
			padded := (rest + sector - 1) / sector * sector
			if cap(d.buf) < padded {
				d.buf = make([]byte, padded)
			}
			if _, err := io.ReadFull(d.r, d.buf[:padded]); err != nil {
				return fmt.Errorf("the VMDK stream ended early: %w", err)
			}
			d.pos += int64(padded / sector)
			comp = append(comp, d.buf[:rest]...)
		}
		zr, err := zlib.NewReader(&sliceReader{b: comp})
		if err != nil {
			return fmt.Errorf("grain at sector %d: %w", val, err)
		}
		if int64(cap(d.grain)) < gb {
			d.grain = make([]byte, gb)
		}
		d.grain = d.grain[:gb]
		n, err := io.ReadFull(zr, d.grain)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return fmt.Errorf("grain at sector %d: %w", val, err)
		}
		clear(d.grain[n:])
		d.at = int64(val) * sector
		if d.at < d.last {
			return errors.New("the grains in the VMDK stream are not in ascending order")
		}
		d.last = d.at + gb
		return nil
	}
}

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

func (s *sliceReader) ReadByte() (byte, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	c := s.b[0]
	s.b = s.b[1:]
	return c, nil
}

// ReadAt reads disk contents. Parts without a grain read as zeros.
func (d *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < d.next {
		return 0, errors.New("a stream-optimized VMDK can only be read front to back")
	}
	size := d.Size()
	var eof error
	if off+int64(len(p)) > size {
		if off >= size {
			return 0, io.EOF
		}
		p, eof = p[:size-off], io.EOF
	}
	clear(p)
	end := off + int64(len(p))
	for !d.done {
		if d.at < 0 {
			if err := d.advance(); err != nil {
				return 0, err
			}
			continue
		}
		gend := d.at + int64(len(d.grain))
		if d.at >= end {
			break
		}
		if gend > off {
			from, to := max(d.at, off), min(gend, end)
			copy(p[from-off:to-off], d.grain[from-d.at:to-d.at])
		}
		if gend > end {
			break // the rest of this grain is for the next call
		}
		d.at = -1
	}
	d.next = end
	return len(p), eof
}

// Drain reads the rest of the stream, checking it, and reports whether
// it ended properly. Call it after the last ReadAt.
func (d *Reader) Drain() error {
	for !d.done {
		if err := d.advance(); err != nil {
			return err
		}
	}
	return nil
}

// descriptor is the text embedded in the disk that describes it.
func descriptor(sectors uint64) []byte {
	cyl := min(sectors/(255*63), 65535)
	return []byte(fmt.Sprintf(`# Disk DescriptorFile
version=1
CID=fffffffe
parentCID=ffffffff
createType="streamOptimized"

# Extent description
RDONLY %d SPARSE "disk.vmdk"

# The Disk Data Base
#DDB

ddb.virtualHWVersion = "13"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "255"
ddb.geometry.sectors = "63"
ddb.adapterType = "lsilogic"
`, sectors, cyl))
}

// A grain is written as a zlib stream of stored, that is uncompressed,
// deflate blocks. That costs space on the wire but makes every grain
// the same size, so the length of the whole stream is known before the
// first byte is sent, which an upload to vSphere needs.
const (
	storedGrain  = 2 + (5 + 65535) + (5 + 1) + 4            // zlib header, two stored blocks, checksum
	grainSectors = (12 + storedGrain + sector - 1) / sector // with its marker
)

// layout works out the stream for a disk of capacity bytes whose
// blocks of blockSize bytes hold data where data says so.
func layout(capacity, blockSize int64, data []bool) (grains, tables, allTables int64, err error) {
	if capacity <= 0 || capacity%sector != 0 {
		return 0, 0, 0, fmt.Errorf("a disk of %d bytes cannot be a VMDK", capacity)
	}
	if blockSize <= 0 || blockSize%grainBytes != 0 {
		return 0, 0, 0, fmt.Errorf("blocks of %d bytes do not line up with VMDK grains", blockSize)
	}
	if int64(len(data)) != (capacity+blockSize-1)/blockSize {
		return 0, 0, 0, errors.New("the block list does not match the disk's size")
	}
	perTable := int64(gtEntries) * grainBytes
	allTables = (capacity + perTable - 1) / perTable
	lastTable := int64(-1)
	for i, has := range data {
		if !has {
			continue
		}
		off := int64(i) * blockSize
		n := min(blockSize, capacity-off)
		grains += (n + grainBytes - 1) / grainBytes
		// A block may straddle two tables only if it is larger than
		// one, which the sizes in use rule out; count both anyway.
		for t := off / perTable; t <= (off+n-1)/perTable; t++ {
			if t != lastTable {
				tables++
				lastTable = t
			}
		}
	}
	return grains, tables, allTables, nil
}

// StreamSize is the exact length in bytes of what a Writer with the
// same arguments produces.
func StreamSize(capacity, blockSize int64, data []bool) (int64, error) {
	grains, tables, allTables, err := layout(capacity, blockSize, data)
	if err != nil {
		return 0, err
	}
	gd := (allTables*4 + sector - 1) / sector
	sectors := int64(overhead) + grains*grainSectors + tables*(1+gtSectors) + (1 + gd) + 2 + 1
	return sectors * sector, nil
}

// Writer produces a stream-optimized disk. Blocks are written with
// WriteAt in ascending order, exactly the ones the data list names.
type Writer struct {
	w         *bufio.Writer
	capacity  int64
	blockSize int64
	data      []bool
	next      int   // the next block that may be written
	pos       int64 // sectors written
	table     int64 // the grain table being filled, or -1
	gt        []uint32
	gd        []uint32
	err       error
}

// NewWriter starts a disk of capacity bytes on w.
func NewWriter(w io.Writer, capacity, blockSize int64, data []bool) (*Writer, error) {
	_, _, allTables, err := layout(capacity, blockSize, data)
	if err != nil {
		return nil, err
	}
	d := &Writer{w: bufio.NewWriterSize(w, 1<<20), capacity: capacity, blockSize: blockSize, data: data, table: -1,
		gt: make([]uint32, gtEntries), gd: make([]uint32, allTables)}
	h := header{Capacity: uint64(capacity / sector), GrainSize: grainSize, DescOffset: 1, DescSize: descSectors, GDOffset: gdAtEnd,
		Overhead: overhead, Flags: flagNewline | flagCompressed | flagMarkers, Compress: 1}
	head := make([]byte, overhead*sector)
	copy(head, h.encode())
	copy(head[sector:], descriptor(h.Capacity))
	d.write(head)
	return d, d.err
}

func (d *Writer) write(b []byte) {
	if d.err != nil {
		return
	}
	if len(b)%sector != 0 {
		d.err = errors.New("vmdk: unaligned write")
		return
	}
	if _, err := d.w.Write(b); err != nil {
		d.err = err
		return
	}
	d.pos += int64(len(b) / sector)
}

func marker(val uint64, typ uint32) []byte {
	m := make([]byte, sector)
	binary.LittleEndian.PutUint64(m, val)
	binary.LittleEndian.PutUint32(m[12:], typ)
	return m
}

// flushTable writes the grain table being filled, if any.
func (d *Writer) flushTable() {
	if d.table < 0 {
		return
	}
	d.write(marker(gtSectors, markerGT))
	d.gd[d.table] = uint32(d.pos)
	b := make([]byte, gtSectors*sector)
	for i, v := range d.gt {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	d.write(b)
	clear(d.gt)
	d.table = -1
}

// WriteAt writes one block. off must be the start of the next block
// that holds data, and p all of that block.
func (d *Writer) WriteAt(p []byte, off int64) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	for d.next < len(d.data) && !d.data[d.next] {
		d.next++
	}
	if d.next >= len(d.data) || off != int64(d.next)*d.blockSize || int64(len(p)) != min(d.blockSize, d.capacity-off) {
		d.err = fmt.Errorf("vmdk: unexpected write of %d bytes at %d", len(p), off)
		return 0, d.err
	}
	d.next++
	out := make([]byte, grainSectors*sector)
	var last [grainBytes]byte
	for at := 0; at < len(p); at += grainBytes {
		g := p[at:min(at+grainBytes, len(p))]
		if len(g) < grainBytes {
			// The disk ends inside this grain; a grain is always whole.
			clear(last[:])
			copy(last[:], g)
			g = last[:]
		}
		lba := uint64(off+int64(at)) / sector
		if t := int64(lba / (grainSize * gtEntries)); t != d.table {
			d.flushTable()
			d.table = t
		}
		if d.pos > 0xffffffff {
			d.err = errors.New("vmdk: the stream is too large for its tables")
			return 0, d.err
		}
		d.gt[lba/grainSize%gtEntries] = uint32(d.pos)
		clear(out)
		binary.LittleEndian.PutUint64(out, lba)
		binary.LittleEndian.PutUint32(out[8:], storedGrain)
		z := out[12:]
		z[0], z[1] = 0x78, 0x01
		z[2] = 0 // not the last block, stored
		binary.LittleEndian.PutUint16(z[3:], 65535)
		binary.LittleEndian.PutUint16(z[5:], 0)
		copy(z[7:], g[:65535])
		z = z[7+65535:]
		z[0] = 1 // the last block, stored
		binary.LittleEndian.PutUint16(z[1:], 1)
		binary.LittleEndian.PutUint16(z[3:], 0xfffe)
		z[5] = g[65535]
		binary.BigEndian.PutUint32(z[6:], adler32.Checksum(g))
		d.write(out)
	}
	return len(p), d.err
}

// Close writes the tables and the footer. The stream is complete only
// if it returns nil.
func (d *Writer) Close() error {
	for d.next < len(d.data) && !d.data[d.next] {
		d.next++
	}
	if d.err == nil && d.next < len(d.data) {
		d.err = fmt.Errorf("vmdk: the disk was closed before block %d was written", d.next)
	}
	d.flushTable()
	gdSectors := (int64(len(d.gd))*4 + sector - 1) / sector
	d.write(marker(uint64(gdSectors), markerGD))
	gdAt := d.pos
	b := make([]byte, gdSectors*sector)
	for i, v := range d.gd {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	d.write(b)
	d.write(marker(1, markerFooter))
	h := header{Capacity: uint64(d.capacity / sector), GrainSize: grainSize, DescOffset: 1, DescSize: descSectors, GDOffset: uint64(gdAt),
		Overhead: overhead, Flags: flagNewline | flagCompressed | flagMarkers, Compress: 1}
	d.write(h.encode())
	d.write(marker(0, markerEOS))
	if d.err == nil {
		d.err = d.w.Flush()
	}
	if d.err != nil {
		return d.err
	}
	d.err = errors.New("vmdk: the disk is closed")
	return nil
}
