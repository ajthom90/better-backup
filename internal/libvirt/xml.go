package libvirt

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// domain is the part of a libvirt domain's XML this package reads.
type domain struct {
	Name string `xml:"name"`
	UUID string `xml:"uuid"`
	OS   struct {
		NVRAM struct {
			Path string `xml:",chardata"`
		} `xml:"nvram"`
	} `xml:"os"`
	Devices struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File string `xml:"file,attr"`
				Dev  string `xml:"dev,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
			ReadOnly  *struct{} `xml:"readonly"`
			Shareable *struct{} `xml:"shareable"`
		} `xml:"disk"`
		TPMs []struct {
			Backend struct {
				Type string `xml:"type,attr"`
			} `xml:"backend"`
		} `xml:"tpm"`
	} `xml:"devices"`
}

func parseDomain(doc []byte) (*domain, error) {
	var d domain
	if err := xml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("reading the machine's definition: %w", err)
	}
	if d.Name == "" {
		return nil, errors.New("the machine's definition has no name")
	}
	return &d, nil
}

// disks sorts a domain's disks into the ones to back up and warnings
// about the ones that cannot be.
func (d *domain) disks() (out []Disk, warnings []string) {
	for _, x := range d.Devices.Disks {
		path := x.Source.File
		if path == "" {
			path = x.Source.Dev
		}
		switch {
		case x.Device == "cdrom" || x.Device == "floppy":
			// Removable media are not part of the machine.
		case x.Target.Dev == "":
		case x.Device != "disk":
			warnings = append(warnings, fmt.Sprintf("the %s %s is passed through to the machine and was not backed up", x.Device, x.Target.Dev))
		case path == "":
			warnings = append(warnings, fmt.Sprintf("the disk %s is not a file or block device on this host (it is of type %q) and was not backed up", x.Target.Dev, x.Type))
		case x.Shareable != nil:
			warnings = append(warnings, fmt.Sprintf("the disk %s is shared with other machines and was not backed up", x.Target.Dev))
		case x.ReadOnly != nil:
			warnings = append(warnings, fmt.Sprintf("the disk %s is read-only and was not backed up", x.Target.Dev))
		default:
			format := x.Driver.Type
			if format == "" {
				format = "raw"
			}
			out = append(out, Disk{Target: x.Target.Dev, File: "disk-" + x.Target.Dev, Path: path, Format: format, Block: x.Type == "block"})
		}
	}
	return out, warnings
}

// swtpm reports whether the machine has an emulated TPM, whose state
// lives outside its disks.
func (d *domain) swtpm() bool {
	for _, t := range d.Devices.TPMs {
		if t.Backend.Type == "emulator" {
			return true
		}
	}
	return false
}

type edit struct {
	from, to int64
	text     string
}

func attr(name, value string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(value))
	return " " + name + "='" + b.String() + "'" // EscapeText escapes quotes too
}

func text(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// rewrite changes a domain's XML for a restore. The machine gets a new
// name; its uuid is dropped unless keepUUID; the disks named in paths
// (by target, such as "vda") become qcow2 files at the given paths; and
// with nvram set the firmware's variable store moves there. Everything
// else is kept byte for byte, since libvirt's XML has more in it than
// this program knows about.
func rewrite(doc []byte, name string, keepUUID bool, paths map[string]string, nvram string) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var stack []string
	var edits []edit
	type span struct{ from, to int64 }
	var (
		start     = map[int]int64{} // depth -> where its element starts
		disk      struct{ open, driver, source span }
		diskDev   string
		backing   []span
		inDisk    bool
		diskAttrs []xml.Attr
		driverAt  []xml.Attr
		named     bool
		nvramFmt  string
	)
	for {
		at := dec.InputOffset()
		tok, err := dec.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the machine's definition: %w", err)
		}
		end := dec.InputOffset()
		path := strings.Join(stack, "/")
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			start[len(stack)] = at
			if path == "domain/devices" && t.Name.Local == "disk" {
				inDisk, diskDev, backing, driverAt = true, "", nil, nil
				disk.open, disk.driver, disk.source = span{at, end}, span{}, span{}
				diskAttrs = t.Attr
			}
			if path == "domain/os" && t.Name.Local == "nvram" {
				for _, a := range t.Attr {
					if a.Name.Local == "format" {
						nvramFmt = attr("format", a.Value)
					}
				}
			}
			if inDisk && path == "domain/devices/disk" {
				switch t.Name.Local {
				case "target":
					for _, a := range t.Attr {
						if a.Name.Local == "dev" {
							diskDev = a.Value
						}
					}
				case "driver":
					driverAt = t.Attr
				}
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("the machine's definition is not well formed")
			}
			from := start[len(stack)]
			stack = stack[:len(stack)-1]
			parent := strings.Join(stack, "/")
			switch {
			case parent == "domain" && t.Name.Local == "name":
				edits = append(edits, edit{from, end, "<name>" + text(name) + "</name>"})
				named = true
			case parent == "domain" && t.Name.Local == "uuid" && !keepUUID:
				edits = append(edits, edit{from, end, ""})
			case parent == "domain/os" && t.Name.Local == "nvram" && nvram != "":
				// Without its template the store is used as it is, which
				// is what a restored one needs.
				edits = append(edits, edit{from, end, "<nvram" + nvramFmt + ">" + text(nvram) + "</nvram>"})
			case parent == "domain/devices/disk" && t.Name.Local == "driver":
				disk.driver = span{from, end}
			case parent == "domain/devices/disk" && t.Name.Local == "source":
				disk.source = span{from, end}
			case parent == "domain/devices/disk" && t.Name.Local == "backingStore":
				backing = append(backing, span{from, end})
			case parent == "domain/devices" && t.Name.Local == "disk":
				inDisk = false
				file, ok := paths[diskDev]
				if !ok {
					break
				}
				open := "<disk type='file'"
				for _, a := range diskAttrs {
					if a.Name.Local != "type" && a.Name.Local != "snapshot" && a.Name.Space == "" {
						open += attr(a.Name.Local, a.Value)
					}
				}
				driver := "<driver"
				hasName := false
				for _, a := range driverAt {
					if a.Name.Local == "name" {
						hasName = true
					}
					if a.Name.Local != "type" && a.Name.Space == "" {
						driver += attr(a.Name.Local, a.Value)
					}
				}
				if !hasName {
					driver += " name='qemu'"
				}
				driver += " type='qcow2'/>"
				source := "<source" + attr("file", file) + "/>"
				if disk.source.to == 0 {
					// Unusual, since a disk without a source was not
					// backed up, but harmless: add one.
					driver += source
				} else {
					edits = append(edits, edit{disk.source.from, disk.source.to, source})
				}
				if disk.driver.to == 0 {
					open += ">" + driver
				} else {
					open += ">"
					edits = append(edits, edit{disk.driver.from, disk.driver.to, driver})
				}
				edits = append(edits, edit{disk.open.from, disk.open.to, open})
				for _, b := range backing {
					edits = append(edits, edit{b.from, b.to, ""})
				}
			}
		}
	}
	if !named {
		return nil, errors.New("the machine's definition has no name")
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].from < edits[j].from })
	var out bytes.Buffer
	pos := int64(0)
	for _, e := range edits {
		if e.from < pos {
			return nil, errors.New("the machine's definition could not be rewritten")
		}
		out.Write(doc[pos:e.from])
		out.WriteString(e.text)
		pos = e.to
	}
	out.Write(doc[pos:])
	return out.Bytes(), nil
}
