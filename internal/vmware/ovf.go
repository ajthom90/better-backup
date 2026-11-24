package vmware

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/ovf"
)

// descriptor is what this package needs to know of an OVF descriptor:
// the description of a machine vSphere writes on export and reads on
// import.
type descriptor struct {
	// Capacity is the size in bytes of the disk kept in each file.
	Capacity map[string]int64
	Networks []string
}

func parseDescriptor(doc string) (*descriptor, error) {
	env, err := ovf.Unmarshal(strings.NewReader(doc))
	if err != nil {
		return nil, fmt.Errorf("reading the machine's OVF description: %w", err)
	}
	d := &descriptor{Capacity: map[string]int64{}}
	files := map[string]string{}
	for _, f := range env.References {
		files[f.ID] = f.Href
	}
	if env.Disk != nil {
		for _, disk := range env.Disk.Disks {
			if disk.FileRef == nil || files[*disk.FileRef] == "" {
				continue
			}
			if n, ok := capacity(disk.Capacity, disk.CapacityAllocationUnits); ok {
				d.Capacity[files[*disk.FileRef]] = n
			}
		}
	}
	if env.Network != nil {
		for _, n := range env.Network.Networks {
			d.Networks = append(d.Networks, n.Name)
		}
	}
	return d, nil
}

// capacity works out a disk size written the OVF way, such as 16 in
// units of "byte * 2^30".
func capacity(value string, units *string) (int64, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || n < 0 {
		return 0, false
	}
	if units != nil {
		u := strings.ToLower(strings.Join(strings.Fields(*units), ""))
		u = strings.TrimPrefix(u, "byte")
		if u != "" {
			if !strings.HasPrefix(u, "*") {
				return 0, false
			}
			base, exp, power := strings.Cut(u[1:], "^")
			b, err := strconv.ParseFloat(base, 64)
			if err != nil {
				return 0, false
			}
			if power {
				e, err := strconv.ParseFloat(exp, 64)
				if err != nil {
					return 0, false
				}
				b = math.Pow(b, e)
			}
			n *= b
		}
	}
	if n > math.MaxInt64/2 || n != math.Trunc(n) {
		return 0, false
	}
	return int64(n), true
}

var (
	fileElement = regexp.MustCompile(`<(?:\w+:)?File\b[^>]*>`)
	sizeAttr    = regexp.MustCompile(`\s(?:\w+:)?size="[^"]*"`)
)
