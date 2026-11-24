// Package disklayout reads and writes the partition tables of disks (GPT
// and MBR), and describes a machine's disks well enough to lay them out
// again on blank hardware. It works on anything that can be read at an
// offset: a physical disk, a virtual disk or a plain file.
package disklayout

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"unicode/utf16"
)

// FileName is the name of the entry that holds a Machine in a snapshot.
const FileName = "machine.json"

const (
	StyleGPT = "gpt"
	StyleMBR = "mbr"

	FirmwareUEFI = "uefi"
	FirmwareBIOS = "bios"
)

// Partition kinds, worked out from the partition type.
const (
	KindESP      = "efi-system"
	KindMSR      = "microsoft-reserved"
	KindRecovery = "windows-recovery"
	KindBIOSBoot = "bios-boot"
	KindSwap     = "swap"
	KindLVM      = "lvm"
	KindRAID     = "raid"
	KindExtended = "extended"
	KindData     = "data"
)

// Machine describes the disks a machine starts from.
type Machine struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	// Firmware is how the machine was started: "uefi" or "bios".
	Firmware string `json:"firmware,omitempty"`
	Disks    []Disk `json:"disks"`
}

// Disk is one disk and its partition table.
type Disk struct {
	// Name identifies the disk inside the backup, such as "disk0".
	Name string `json:"name"`
	// Device is what the disk was called on the machine.
	Device     string `json:"device,omitempty"`
	Model      string `json:"model,omitempty"`
	Size       int64  `json:"size"`
	SectorSize int    `json:"sector_size"`
	Style      string `json:"style"`
	// GUID is a GPT disk's identifier. Boot configuration refers to it,
	// so it is put back as it was.
	GUID string `json:"guid,omitempty"`
	// Head is the start of the disk exactly as it was. On an MBR disk it
	// runs up to the first partition and so holds the boot code, the
	// disk signature, the partition table and anything a boot loader
	// keeps behind it. On a GPT disk it is the first sector.
	Head []byte `json:"head,omitempty"`
	// EBRs are the extended boot records of an MBR disk, which chain its
	// logical partitions together.
	EBRs []Sector `json:"ebrs,omitempty"`
	// FirstUsable and Entries describe the GPT itself.
	FirstUsable int64       `json:"first_usable,omitempty"`
	Entries     int         `json:"entries,omitempty"`
	Partitions  []Partition `json:"partitions"`
}

// Sector is a sector of a disk kept as it was.
type Sector struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

// Partition is one entry of a partition table.
type Partition struct {
	Number int   `json:"number"`
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
	// Type is the GPT type GUID, or the MBR type as two hex digits.
	Type string `json:"type"`
	Kind string `json:"kind"`
	// GUID, Name and Attrs are a GPT partition's identity.
	GUID  string `json:"guid,omitempty"`
	Name  string `json:"name,omitempty"`
	Attrs uint64 `json:"attrs,omitempty"`
	// Active marks the MBR partition the machine starts from.
	Active  bool `json:"active,omitempty"`
	Logical bool `json:"logical,omitempty"`

	// What the partition holds, for people choosing what to restore.
	FS    string `json:"fs,omitempty"`
	Label string `json:"label,omitempty"`
	Mount string `json:"mount,omitempty"` // drive letter or mount point
	// Image names the image of this partition in the backup. Empty: the
	// partition is made again but its contents were not saved.
	Image string `json:"image,omitempty"`
}

// End is the offset just behind the partition.
func (p *Partition) End() int64 { return p.Offset + p.Size }

var gptKinds = map[string]string{
	"C12A7328-F81F-11D2-BA4B-00A0C93EC93B": KindESP,
	"E3C9E316-0B5C-4DB8-817D-F92DF00215AE": KindMSR,
	"DE94BBA4-06D1-4D40-A16A-BFD50179D6AC": KindRecovery,
	"21686148-6449-6E6F-744E-656564454649": KindBIOSBoot,
	"0657FD6D-A4AB-43C4-84E5-0933C84B4F4F": KindSwap,
	"E6D6D379-F507-44C2-A23C-238F2A3DF928": KindLVM,
	"A19D880F-05FC-4D3B-A006-743F0F84911E": KindRAID,
}

var mbrKinds = map[byte]string{
	0xEF: KindESP, 0x27: KindRecovery, 0x82: KindSwap, 0x8E: KindLVM, 0xFD: KindRAID,
	0x05: KindExtended, 0x0F: KindExtended, 0x85: KindExtended,
}

// GUIDString formats the 16 bytes of an on-disk GUID.
func GUIDString(b []byte) string {
	return strings.ToUpper(fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint16(b[4:]), binary.LittleEndian.Uint16(b[6:]), b[8:10], b[10:16]))
}

// ParseGUID returns the on-disk form of a GUID string.
func ParseGUID(s string) ([16]byte, error) {
	var g [16]byte
	var a uint32
	var b, c uint16
	var d, e []byte
	if _, err := fmt.Sscanf(s, "%08x-%04x-%04x-%04x-%012x", &a, &b, &c, &d, &e); err != nil || len(d) != 2 || len(e) != 6 || len(s) != 36 {
		return g, fmt.Errorf("%q is not a GUID", s)
	}
	binary.LittleEndian.PutUint32(g[0:], a)
	binary.LittleEndian.PutUint16(g[4:], b)
	binary.LittleEndian.PutUint16(g[6:], c)
	copy(g[8:], d)
	copy(g[10:], e)
	return g, nil
}

// maxHead bounds how much of the start of an MBR disk is kept.
const maxHead = 1 << 20

func readFull(r io.ReaderAt, off int64, n int) ([]byte, error) {
	b := make([]byte, n)
	if got, err := r.ReadAt(b, off); got != n {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("reading %d bytes at offset %d: %w", n, off, err)
	}
	return b, nil
}

// Read reads the partition table of a disk of the given size. sectorSize
// is the disk's logical sector size; 0 means find it out, which works for
// GPT disks and assumes 512 bytes for MBR disks.
func Read(r io.ReaderAt, size int64, sectorSize int) (*Disk, error) {
	if size < 1<<20 {
		return nil, errors.New("the disk is smaller than 1 MiB")
	}
	first, err := readFull(r, 0, 4096)
	if err != nil {
		return nil, err
	}
	sizes := []int{sectorSize}
	if sectorSize == 0 {
		sizes = []int{512, 4096}
	}
	for _, ss := range sizes {
		hdr, err := readFull(r, int64(ss), ss)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(hdr[:8], []byte("EFI PART")) {
			return readGPT(r, size, ss, first, hdr)
		}
	}
	if first[510] != 0x55 || first[511] != 0xAA {
		return nil, errors.New("the disk has no partition table")
	}
	if sectorSize == 0 {
		sectorSize = 512
	}
	return readMBR(r, size, sectorSize, first)
}

func readGPT(r io.ReaderAt, size int64, ss int, first, hdr []byte) (*Disk, error) {
	le := binary.LittleEndian
	hsize := le.Uint32(hdr[12:])
	if hsize < 92 || int(hsize) > ss {
		return nil, errors.New("the GPT header has an impossible size")
	}
	sum := le.Uint32(hdr[16:])
	chk := append([]byte(nil), hdr[:hsize]...)
	le.PutUint32(chk[16:], 0)
	if crc32.ChecksumIEEE(chk) != sum {
		return nil, errors.New("the GPT header is damaged (checksum mismatch)")
	}
	d := &Disk{Size: size, SectorSize: ss, Style: StyleGPT, Head: append([]byte(nil), first[:ss]...),
		GUID: GUIDString(hdr[56:72]), FirstUsable: int64(le.Uint64(hdr[40:])), Entries: int(le.Uint32(hdr[80:]))}
	entLBA, esize := int64(le.Uint64(hdr[72:])), int(le.Uint32(hdr[84:]))
	if d.Entries <= 0 || d.Entries > 1024 || esize < 128 || esize > 4096 {
		return nil, errors.New("the GPT header describes an impossible partition table")
	}
	n := (d.Entries*esize + ss - 1) / ss * ss
	ents, err := readFull(r, entLBA*int64(ss), n)
	if err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(ents[:d.Entries*esize]) != le.Uint32(hdr[88:]) {
		return nil, errors.New("the GPT partition table is damaged (checksum mismatch)")
	}
	for i := 0; i < d.Entries; i++ {
		e := ents[i*esize : (i+1)*esize]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		firstLBA, lastLBA := int64(le.Uint64(e[32:])), int64(le.Uint64(e[40:]))
		if firstLBA <= 0 || lastLBA < firstLBA || (lastLBA+1)*int64(ss) > size {
			return nil, fmt.Errorf("GPT partition %d lies outside the disk", i+1)
		}
		u := make([]uint16, 0, 36)
		for k := 56; k+1 < 128; k += 2 {
			c := le.Uint16(e[k:])
			if c == 0 {
				break
			}
			u = append(u, c)
		}
		p := Partition{Number: i + 1, Offset: firstLBA * int64(ss), Size: (lastLBA - firstLBA + 1) * int64(ss),
			Type: GUIDString(e[:16]), GUID: GUIDString(e[16:32]), Attrs: le.Uint64(e[48:]), Name: string(utf16.Decode(u))}
		if p.Kind = gptKinds[p.Type]; p.Kind == "" {
			p.Kind = KindData
		}
		d.Partitions = append(d.Partitions, p)
	}
	return d, nil
}

func readMBR(r io.ReaderAt, size int64, ss int, first []byte) (*Disk, error) {
	le := binary.LittleEndian
	d := &Disk{Size: size, SectorSize: ss, Style: StyleMBR}
	firstStart := int64(maxHead)
	add := func(num int, e []byte, base int64, logical bool) (*Partition, error) {
		start, count := base+int64(le.Uint32(e[8:]))*int64(ss), int64(le.Uint32(e[12:]))*int64(ss)
		if start <= 0 || count <= 0 || start+count > size {
			return nil, fmt.Errorf("MBR partition %d lies outside the disk", num)
		}
		p := Partition{Number: num, Offset: start, Size: count, Type: fmt.Sprintf("%02X", e[4]), Active: e[0] == 0x80, Logical: logical}
		if p.Kind = mbrKinds[e[4]]; p.Kind == "" {
			p.Kind = KindData
		}
		d.Partitions = append(d.Partitions, p)
		firstStart = min(firstStart, start)
		return &d.Partitions[len(d.Partitions)-1], nil
	}
	var ext *Partition
	for i := 0; i < 4; i++ {
		e := first[446+16*i : 462+16*i]
		if e[4] == 0 {
			continue
		}
		p, err := add(i+1, e, 0, false)
		if err != nil {
			return nil, err
		}
		if p.Kind == KindExtended && ext == nil {
			cp := *p
			ext = &cp
		}
	}
	// Logical partitions: each extended boot record names one partition,
	// relative to itself, and the next record, relative to the extended
	// partition.
	if ext != nil {
		ebr := ext.Offset
		for num := 5; num < 5+128; num++ {
			sec, err := readFull(r, ebr, ss)
			if err != nil {
				return nil, err
			}
			if sec[510] != 0x55 || sec[511] != 0xAA {
				break
			}
			d.EBRs = append(d.EBRs, Sector{Offset: ebr, Data: sec})
			if e := sec[446:462]; e[4] != 0 {
				if _, err := add(num, e, ebr, true); err != nil {
					return nil, err
				}
			}
			next := sec[462:478]
			if next[4] == 0 || le.Uint32(next[8:]) == 0 {
				break
			}
			ebr = ext.Offset + int64(le.Uint32(next[8:]))*int64(ss)
			if ebr >= ext.End() {
				return nil, errors.New("the chain of logical partitions leaves the extended partition")
			}
		}
	}
	n := int(firstStart) / ss * ss
	if len(d.Partitions) == 0 || n < ss {
		n = ss
	}
	head, err := readFull(r, 0, n)
	if err != nil {
		return nil, err
	}
	d.Head = head
	return d, nil
}

// Needs is the smallest disk the layout fits on.
func (d *Disk) Needs() int64 {
	var end int64
	for i := range d.Partitions {
		end = max(end, d.Partitions[i].End())
	}
	if d.Style == StyleGPT {
		// The spare copy of the table sits behind the last partition.
		end += int64(d.entrySectors()+1) * int64(d.SectorSize)
	}
	return max(end, int64(len(d.Head)))
}

func (d *Disk) entrySectors() int {
	n := d.Entries
	if n <= 0 {
		n = 128
	}
	return (n*128 + d.SectorSize - 1) / d.SectorSize
}

// Check reports why the layout cannot go onto a disk of the given size
// and sector size.
func (d *Disk) Check(size int64, sectorSize int) error {
	if sectorSize != 0 && sectorSize != d.SectorSize {
		return fmt.Errorf("the disk has %d-byte sectors and the backup was taken from a disk with %d-byte sectors; they must match", sectorSize, d.SectorSize)
	}
	if need := d.Needs(); size < need {
		return fmt.Errorf("the disk holds %d bytes and the backup needs at least %d", size, need)
	}
	if d.Style == StyleMBR && size > int64(d.SectorSize)<<32 && d.Size <= int64(d.SectorSize)<<32 {
		return nil // fine: the space past what an MBR can address stays unused
	}
	return nil
}

// Write puts the partition table onto a disk of the given size. The
// partitions keep their places and identities; on a larger disk the space
// behind them is left free. Call it after the partitions' contents are
// written, so that a disk that was only partly restored has no table.
func (d *Disk) Write(w io.WriterAt, size int64) error {
	if err := d.Check(size, 0); err != nil {
		return err
	}
	ss := int64(d.SectorSize)
	if d.Style == StyleMBR {
		for _, s := range d.EBRs {
			if _, err := w.WriteAt(s.Data, s.Offset); err != nil {
				return err
			}
		}
		if len(d.Head) < 512 {
			return errors.New("the backup holds no boot sector for this disk")
		}
		_, err := w.WriteAt(d.Head, 0)
		return err
	}
	if d.Style != StyleGPT {
		return fmt.Errorf("unknown partition table style %q", d.Style)
	}
	le := binary.LittleEndian
	entries := d.Entries
	if entries <= 0 {
		entries = 128
	}
	es := int64(d.entrySectors())
	sectors := size / ss
	firstUsable := max(d.FirstUsable, 2+es)
	lastUsable := sectors - es - 2
	table := make([]byte, es*ss)
	for i := range d.Partitions {
		p := &d.Partitions[i]
		if p.Number < 1 || p.Number > entries {
			return fmt.Errorf("partition number %d does not fit the table", p.Number)
		}
		if p.Offset%ss != 0 || p.Size%ss != 0 || p.Offset/ss < firstUsable || p.End()/ss-1 > lastUsable {
			return fmt.Errorf("partition %d does not fit the disk", p.Number)
		}
		e := table[(p.Number-1)*128 : p.Number*128]
		tg, err := ParseGUID(p.Type)
		if err != nil {
			return err
		}
		ug, err := ParseGUID(p.GUID)
		if err != nil {
			return err
		}
		copy(e[0:], tg[:])
		copy(e[16:], ug[:])
		le.PutUint64(e[32:], uint64(p.Offset/ss))
		le.PutUint64(e[40:], uint64(p.End()/ss-1))
		le.PutUint64(e[48:], p.Attrs)
		for k, c := range utf16.Encode([]rune(p.Name)) {
			if k >= 36 {
				break
			}
			le.PutUint16(e[56+2*k:], c)
		}
	}
	dg, err := ParseGUID(d.GUID)
	if err != nil {
		return err
	}
	header := func(my, alt, ents int64) []byte {
		h := make([]byte, ss)
		copy(h, "EFI PART")
		le.PutUint32(h[8:], 0x00010000)
		le.PutUint32(h[12:], 92)
		le.PutUint64(h[24:], uint64(my))
		le.PutUint64(h[32:], uint64(alt))
		le.PutUint64(h[40:], uint64(firstUsable))
		le.PutUint64(h[48:], uint64(lastUsable))
		copy(h[56:], dg[:])
		le.PutUint64(h[72:], uint64(ents))
		le.PutUint32(h[80:], uint32(entries))
		le.PutUint32(h[84:], 128)
		le.PutUint32(h[88:], crc32.ChecksumIEEE(table[:entries*128]))
		le.PutUint32(h[16:], crc32.ChecksumIEEE(h[:92]))
		return h
	}
	// The protective MBR keeps the boot code the disk had and claims the
	// whole disk, or as much as an MBR can describe. On a disk of the
	// original size it goes back exactly as it was.
	mbr := make([]byte, ss)
	if len(d.Head) >= 512 && d.Head[510] == 0x55 && d.Head[511] == 0xAA && d.Head[450] == 0xEE && size == d.Size {
		copy(mbr, d.Head)
	} else {
		if len(d.Head) >= 440 {
			copy(mbr, d.Head[:440])
		}
		pe := mbr[446:462]
		pe[1], pe[2], pe[3] = 0x00, 0x02, 0x00
		pe[4] = 0xEE
		pe[5], pe[6], pe[7] = 0xFF, 0xFF, 0xFF
		le.PutUint32(pe[8:], 1)
		le.PutUint32(pe[12:], uint32(min(sectors-1, 0xFFFFFFFF)))
		mbr[510], mbr[511] = 0x55, 0xAA
	}
	// Spare copy first, then the entries, then the header that makes the
	// table valid, and the protective MBR last.
	for _, part := range []struct {
		data []byte
		lba  int64
	}{
		{table, sectors - es - 1},
		{header(sectors-1, 1, sectors-es-1), sectors - 1},
		{table, 2},
		{header(1, sectors-1, 2), 1},
		{mbr, 0},
	} {
		if _, err := w.WriteAt(part.data, part.lba*ss); err != nil {
			return err
		}
	}
	return nil
}

// Summary describes a disk's table in a few words, for people choosing
// a disk to restore onto.
func (d *Disk) Summary() string {
	if len(d.Partitions) == 0 {
		return strings.ToUpper(d.Style) + ", no partitions"
	}
	var names []string
	for i := range d.Partitions {
		p := &d.Partitions[i]
		switch {
		case p.Label != "":
			names = append(names, p.Label)
		case p.Name != "":
			names = append(names, p.Name)
		case p.FS != "":
			names = append(names, p.FS)
		}
	}
	s := fmt.Sprintf("%s, %d partitions", strings.ToUpper(d.Style), len(d.Partitions))
	if len(names) > 0 {
		s += " (" + strings.Join(names, ", ") + ")"
	}
	return s
}

// Sniff recognizes the file system at the start of a partition from its
// first 128 KiB or so. It returns "" when it does not know it.
func Sniff(r io.ReaderAt) (fs, label string) {
	b := make([]byte, 128<<10)
	n, _ := r.ReadAt(b, 0)
	b = b[:n]
	has := func(off int, magic string) bool {
		return len(b) >= off+len(magic) && string(b[off:off+len(magic)]) == magic
	}
	trim := func(x []byte) string { return strings.TrimRight(string(bytes.TrimRight(x, "\x00")), " ") }
	switch {
	case has(3, "NTFS    "):
		return "NTFS", ""
	case has(3, "EXFAT   "):
		return "exFAT", ""
	case has(0, "ReFS\x00"):
		return "ReFS", ""
	case has(82, "FAT32   "):
		return "FAT32", trim(b[71:82])
	case has(54, "FAT16   "), has(54, "FAT12   "), has(54, "FAT     "):
		return "FAT", trim(b[43:54])
	case has(0, "XFSB"):
		return "xfs", trim(b[108:120])
	case has(0x10040, "_BHRfS_M"):
		return "btrfs", trim(b[0x1012b : 0x1012b+64])
	case has(536, "LVM2 001"), has(1048, "LVM2 001"):
		return "LVM", ""
	case has(0, "LUKS\xba\xbe"):
		return "LUKS", ""
	case has(4086, "SWAPSPACE2"):
		return "swap", trim(b[1052:1068])
	case len(b) >= 1024+136 && b[1024+56] == 0x53 && b[1024+57] == 0xEF:
		fs = "ext2"
		compat, incompat := binary.LittleEndian.Uint32(b[1024+92:]), binary.LittleEndian.Uint32(b[1024+96:])
		if compat&0x4 != 0 {
			fs = "ext3"
		}
		if incompat&0x2C0 != 0 { // extents, 64bit, flex_bg
			fs = "ext4"
		}
		return fs, trim(b[1024+120 : 1024+136])
	}
	return "", ""
}
