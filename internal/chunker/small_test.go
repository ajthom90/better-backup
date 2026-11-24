package chunker

import (
	"bytes"
	"io"
	"testing"
)

// BenchmarkSmallFiles chunks many small files, as a backup of a file
// server does.
func BenchmarkSmallFiles(b *testing.B) {
	data := bytes.Repeat([]byte("x"), 4096)
	for i := 0; i < b.N; i++ {
		c := New(bytes.NewReader(data))
		for {
			if _, err := c.Next(); err == io.EOF {
				break
			}
		}
		c.Release()
	}
}

// TestReleaseKeepsStreamsApart reads two streams one after the other
// through the same buffer.
func TestReleaseKeepsStreamsApart(t *testing.T) {
	for _, want := range []string{"first stream, the longer of the two", "second"} {
		c := New(bytes.NewReader([]byte(want)))
		got, err := c.Next()
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
		if _, err := c.Next(); err != io.EOF {
			t.Fatalf("after the only chunk: %v", err)
		}
		c.Release()
		c.Release() // twice does no harm
	}
}
