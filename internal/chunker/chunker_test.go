package chunker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"testing"
)

func split(t *testing.T, data []byte) [][]byte {
	t.Helper()
	c := New(bytes.NewReader(data))
	var out [][]byte
	for {
		b, err := c.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), b...))
	}
}

func randData(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestRoundTripAndBounds(t *testing.T) {
	data := randData(1, 64<<20)
	chunks := split(t, data)
	var joined []byte
	for i, c := range chunks {
		if len(c) > MaxSize {
			t.Fatalf("chunk %d too large: %d", i, len(c))
		}
		if i < len(chunks)-1 && len(c) < MinSize {
			t.Fatalf("chunk %d too small: %d", i, len(c))
		}
		joined = append(joined, c...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("chunks do not reassemble to the input")
	}
	avg := len(data) / len(chunks)
	if avg < AvgSize/2 || avg > AvgSize*2 {
		t.Fatalf("average chunk size %d is far from %d", avg, AvgSize)
	}
}

func TestShiftResistance(t *testing.T) {
	data := randData(2, 48<<20)
	shifted := append(append([]byte("inserted bytes"), data[:1000]...), data[1000:]...)
	seen := map[[32]byte]bool{}
	for _, c := range split(t, data) {
		seen[sha256.Sum256(c)] = true
	}
	var total, shared int
	for _, c := range split(t, shifted) {
		total++
		if seen[sha256.Sum256(c)] {
			shared++
		}
	}
	if shared < total*8/10 {
		t.Fatalf("only %d of %d chunks survived an insertion", shared, total)
	}
}

func TestEmptyAndSmall(t *testing.T) {
	if n := len(split(t, nil)); n != 0 {
		t.Fatalf("empty input gave %d chunks", n)
	}
	if c := split(t, []byte("hi")); len(c) != 1 || string(c[0]) != "hi" {
		t.Fatalf("small input: %q", c)
	}
}

// The boundaries are part of the on-disk format. This fails if the gear
// table, masks or size limits change.
func TestBoundariesAreStable(t *testing.T) {
	h := sha256.New()
	for _, c := range split(t, randData(3, 32<<20)) {
		var l [4]byte
		l[0], l[1], l[2], l[3] = byte(len(c)>>24), byte(len(c)>>16), byte(len(c)>>8), byte(len(c))
		h.Write(l[:])
	}
	got := hex.EncodeToString(h.Sum(nil))
	const want = "31bf47e7bfda6966b3d0d9ede3c92b3c5a2440f005a3ed3fb40e6dba7d038713"
	if got != want {
		t.Fatalf("chunk boundaries changed: got %s", got)
	}
}
