// Package chunker splits a byte stream into content-defined chunks using
// FastCDC with normalized chunking, so an insertion early in a file does
// not shift every later chunk boundary.
package chunker

import "io"

const (
	MinSize = 512 << 10
	AvgSize = 1 << 20
	MaxSize = 8 << 20

	// Normalized chunking: a harder mask before the average size and an
	// easier one after it pulls chunk sizes toward the average.
	maskS = uint64(1<<22-1) << 42
	maskL = uint64(1<<18-1) << 46
)

// gear is the fixed table for the rolling hash. It is part of the storage
// format: changing it changes every chunk boundary and defeats dedup
// against existing repositories.
var gear = func() (t [256]uint64) {
	x := uint64(0x6262_6b75_7031_2e30) // splitmix64, fixed seed
	for i := range t {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		t[i] = z ^ (z >> 31)
	}
	return
}()

// Chunker reads chunks from a stream.
type Chunker struct {
	r          io.Reader
	buf        []byte
	start, end int
	eof        bool
}

// New returns a Chunker reading from r.
func New(r io.Reader) *Chunker {
	return &Chunker{r: r, buf: make([]byte, 4*MaxSize)}
}

// Next returns the next chunk. The slice is only valid until the following
// call. It returns io.EOF after the last chunk; an empty stream yields no
// chunks.
func (c *Chunker) Next() ([]byte, error) {
	if c.end-c.start < MaxSize && !c.eof {
		if c.start > 0 {
			copy(c.buf, c.buf[c.start:c.end])
			c.end -= c.start
			c.start = 0
		}
		for c.end < len(c.buf) {
			n, err := c.r.Read(c.buf[c.end:])
			c.end += n
			if err == io.EOF {
				c.eof = true
				break
			}
			if err != nil {
				return nil, err
			}
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := cut(c.buf[c.start:c.end])
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}

// cut returns the length of the first chunk in data.
func cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}
	if n > MaxSize {
		n = MaxSize
	}
	normal := AvgSize
	if normal > n {
		normal = n
	}
	var fp uint64
	i := MinSize
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&maskL == 0 {
			return i + 1
		}
	}
	return n
}
