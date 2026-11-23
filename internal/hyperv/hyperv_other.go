//go:build !windows

package hyperv

// Checkpoint is a frozen view of a virtual machine's disks.
type Checkpoint struct {
	Config   Config
	Warnings []string
}

func List() ([]string, error)                               { return nil, ErrUnsupported }
func Prepare(name, tag string) (*Checkpoint, error)         { return nil, ErrUnsupported }
func (c *Checkpoint) Release() error                        { return nil }
func Create(cfg Config, dir, name string) ([]string, error) { return nil, ErrUnsupported }
