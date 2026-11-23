//go:build unix

package backup

import (
	"fmt"
	"io"
	"os"
)

// OpenImage opens a block device or disk image file for an image
// backup. name is recorded in the snapshot and readPath is read.
func OpenImage(name, readPath string) (ImageSource, error) {
	f, err := os.Open(readPath)
	if err != nil {
		return ImageSource{}, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err == nil && size == 0 {
		err = fmt.Errorf("%s is empty or its size cannot be read", readPath)
	}
	if err != nil {
		f.Close()
		return ImageSource{}, err
	}
	return ImageSource{Name: name, Size: size, R: f, Close: func() { f.Close() }}, nil
}
