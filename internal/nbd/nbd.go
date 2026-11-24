// Package nbd is a small client for the Network Block Device protocol,
// which QEMU speaks to hand a disk's contents to a backup program. It
// reads and writes blocks and asks which parts of a disk hold data or
// were written since a point in time.
package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	magicInit   = 0x4e42444d41474943 // "NBDMAGIC"
	magicOpt    = 0x49484156454f5054 // "IHAVEOPT"
	magicOptRep = 0x3e889045565a9
	magicReq    = 0x25609513
	magicSimple = 0x67446698
	magicStruct = 0x668e33ef

	optGo         = 7
	optStructured = 8
	optSetMeta    = 10

	repAck  = 1
	repInfo = 3
	repMeta = 4

	cmdRead        = 0
	cmdWrite       = 1
	cmdDisc        = 2
	cmdFlush       = 3
	cmdBlockStatus = 7

	chunkData   = 1
	chunkHole   = 2
	chunkStatus = 5

	// maxIO is the largest read or write sent in one request. QEMU
	// accepts up to 32 MiB.
	maxIO = 16 << 20
	// maxStatus is the longest range one status request asks about.
	maxStatus = 1 << 30
)

// Allocation is the name of the context that says which parts of a
// disk hold data. DirtyBitmap names the context for a QEMU bitmap.
const Allocation = "base:allocation"

func DirtyBitmap(name string) string { return "qemu:dirty-bitmap:" + name }

// Extent is a run of bytes that share a status.
type Extent struct {
	Off, Len int64
	// Flags are the context's status bits. For Allocation bit 0 means
	// a hole and bit 1 means the range reads as zeros; for a dirty
	// bitmap bit 0 means the range was written.
	Flags uint32
}

// Client is a connection to one export. Its methods may be called from
// several goroutines; requests are sent one at a time.
type Client struct {
	mu       sync.Mutex
	c        net.Conn
	size     int64
	flags    uint16
	contexts map[string]uint32
	cookie   uint64
	err      error // set once the connection is unusable
}

// Dial connects to an NBD server and opens an export. network and addr
// are as for net.Dial; "unix" and a socket path is the usual pair.
// contexts lists the status contexts to ask for; the ones the server
// does not have are left out, see Has.
func Dial(network, addr, export string, contexts ...string) (*Client, error) {
	conn, err := net.DialTimeout(network, addr, 30*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{c: conn, contexts: map[string]uint32{}}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	if err := c.handshake(export, contexts); err != nil {
		conn.Close()
		return nil, fmt.Errorf("nbd %s: %w", addr, err)
	}
	conn.SetDeadline(time.Time{})
	return c, nil
}

func (c *Client) handshake(export string, contexts []string) error {
	var hello [18]byte
	if _, err := io.ReadFull(c.c, hello[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint64(hello[0:]) != magicInit || binary.BigEndian.Uint64(hello[8:]) != magicOpt {
		return errors.New("the server does not speak the NBD protocol this client knows")
	}
	if binary.BigEndian.Uint16(hello[16:])&1 == 0 {
		return errors.New("the server is too old (no fixed newstyle negotiation)")
	}
	// Fixed newstyle, and no padding after the export information.
	if _, err := c.c.Write([]byte{0, 0, 0, 3}); err != nil {
		return err
	}
	// Structured replies carry holes and block status.
	structured := false
	if err := c.option(optStructured, nil, func(typ uint32, _ []byte) error { return nil }); err == nil {
		structured = true
	} else if !isRefusal(err) {
		return err
	}
	name := make([]byte, 4, 4+len(export))
	binary.BigEndian.PutUint32(name, uint32(len(export)))
	name = append(name, export...)
	if structured && len(contexts) > 0 {
		req := append([]byte{}, name...)
		req = binary.BigEndian.AppendUint32(req, uint32(len(contexts)))
		for _, q := range contexts {
			req = binary.BigEndian.AppendUint32(req, uint32(len(q)))
			req = append(req, q...)
		}
		err := c.option(optSetMeta, req, func(typ uint32, data []byte) error {
			if typ == repMeta && len(data) >= 4 {
				c.contexts[string(data[4:])] = binary.BigEndian.Uint32(data)
			}
			return nil
		})
		if err != nil && !isRefusal(err) {
			return err
		}
	}
	gotSize := false
	err := c.option(optGo, append(name, 0, 0), func(typ uint32, data []byte) error {
		if typ == repInfo && len(data) >= 12 && binary.BigEndian.Uint16(data) == 0 {
			c.size = int64(binary.BigEndian.Uint64(data[2:]))
			c.flags = binary.BigEndian.Uint16(data[10:])
			gotSize = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("opening the export %q: %w", export, err)
	}
	if !gotSize {
		return fmt.Errorf("the server did not describe the export %q", export)
	}
	if !structured {
		c.contexts = map[string]uint32{}
	}
	return nil
}

type refusal struct {
	code uint32
	msg  string
}

func (r refusal) Error() string {
	if r.msg != "" {
		return r.msg
	}
	return fmt.Sprintf("the server refused (code %d)", r.code&0x7fffffff)
}

func isRefusal(err error) bool {
	var r refusal
	return errors.As(err, &r)
}

// option sends one option and hands each reply before the final
// acknowledgement to each.
func (c *Client) option(opt uint32, data []byte, each func(typ uint32, data []byte) error) error {
	req := make([]byte, 16, 16+len(data))
	binary.BigEndian.PutUint64(req, magicOpt)
	binary.BigEndian.PutUint32(req[8:], opt)
	binary.BigEndian.PutUint32(req[12:], uint32(len(data)))
	if _, err := c.c.Write(append(req, data...)); err != nil {
		return err
	}
	for {
		var h [20]byte
		if _, err := io.ReadFull(c.c, h[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint64(h[0:]) != magicOptRep || binary.BigEndian.Uint32(h[8:]) != opt {
			return errors.New("unexpected reply while negotiating")
		}
		typ, n := binary.BigEndian.Uint32(h[12:]), binary.BigEndian.Uint32(h[16:])
		if n > 1<<20 {
			return errors.New("oversized reply while negotiating")
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(c.c, body); err != nil {
			return err
		}
		switch {
		case typ&(1<<31) != 0:
			return refusal{typ, string(body)}
		case typ == repAck:
			return nil
		default:
			if err := each(typ, body); err != nil {
				return err
			}
		}
	}
}

// Size is the export's size in bytes.
func (c *Client) Size() int64 { return c.size }

// ReadOnly reports whether the export refuses writes.
func (c *Client) ReadOnly() bool { return c.flags&2 != 0 }

// Has reports whether the server offers a status context.
func (c *Client) Has(context string) bool {
	_, ok := c.contexts[context]
	return ok
}

// send writes one request. The caller holds c.mu.
func (c *Client) send(cmd uint16, off int64, length uint32, payload []byte) (uint64, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.cookie++
	req := make([]byte, 28, 28+len(payload))
	binary.BigEndian.PutUint32(req, magicReq)
	binary.BigEndian.PutUint16(req[6:], cmd)
	binary.BigEndian.PutUint64(req[8:], c.cookie)
	binary.BigEndian.PutUint64(req[16:], uint64(off))
	binary.BigEndian.PutUint32(req[24:], length)
	if _, err := c.c.Write(append(req, payload...)); err != nil {
		return 0, c.fail(err)
	}
	return c.cookie, nil
}

func (c *Client) fail(err error) error {
	if c.err == nil {
		c.err = fmt.Errorf("nbd connection lost: %w", err)
	}
	return c.err
}

// errno turns an NBD error number into an error.
func errno(n uint32, msg string) error {
	if msg != "" {
		return fmt.Errorf("nbd server: %s", msg)
	}
	names := map[uint32]string{1: "operation not permitted", 5: "input/output error", 12: "out of memory", 22: "invalid request",
		28: "no space left on the device", 75: "value too large", 95: "operation not supported", 108: "the server is shutting down"}
	if s, ok := names[n]; ok {
		return errors.New("nbd server: " + s)
	}
	return fmt.Errorf("nbd server: error %d", n)
}

// reply reads the replies to one request. chunk gets every structured
// chunk that is not an error.
func (c *Client) reply(cookie uint64, chunk func(typ uint16, n uint32) error) error {
	var result error
	for {
		var h [20]byte
		if _, err := io.ReadFull(c.c, h[:4]); err != nil {
			return c.fail(err)
		}
		switch binary.BigEndian.Uint32(h[:]) {
		case magicSimple:
			if _, err := io.ReadFull(c.c, h[4:16]); err != nil {
				return c.fail(err)
			}
			if binary.BigEndian.Uint64(h[8:]) != cookie {
				return c.fail(errors.New("reply to a different request"))
			}
			if e := binary.BigEndian.Uint32(h[4:]); e != 0 {
				return errno(e, "")
			}
			if chunk != nil {
				// A simple reply to a read is followed by the data.
				return chunk(0, 0)
			}
			return nil
		case magicStruct:
			if _, err := io.ReadFull(c.c, h[4:20]); err != nil {
				return c.fail(err)
			}
			flags, typ := binary.BigEndian.Uint16(h[4:]), binary.BigEndian.Uint16(h[6:])
			n := binary.BigEndian.Uint32(h[16:])
			if binary.BigEndian.Uint64(h[8:]) != cookie {
				return c.fail(errors.New("reply to a different request"))
			}
			switch {
			case typ&0x8000 != 0:
				body := make([]byte, min(n, 1<<16))
				if _, err := io.ReadFull(c.c, body); err != nil {
					return c.fail(err)
				}
				if _, err := io.CopyN(io.Discard, c.c, int64(n)-int64(len(body))); err != nil {
					return c.fail(err)
				}
				if len(body) >= 6 {
					msg := ""
					if l := int(binary.BigEndian.Uint16(body[4:])); 6+l <= len(body) {
						msg = string(body[6 : 6+l])
					}
					result = errno(binary.BigEndian.Uint32(body), msg)
				} else {
					result = errors.New("nbd server: the request failed")
				}
			case typ == 0 || chunk == nil:
				if _, err := io.CopyN(io.Discard, c.c, int64(n)); err != nil {
					return c.fail(err)
				}
			default:
				if err := chunk(typ, n); err != nil {
					return c.fail(err)
				}
			}
			if flags&1 != 0 {
				return result
			}
		default:
			return c.fail(errors.New("unexpected data from the server"))
		}
	}
}

// ReadAt reads from the export. Parts the server reports as holes read
// as zeros.
func (c *Client) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("nbd: negative offset")
	}
	total := 0
	var eof error
	if off+int64(len(p)) > c.size {
		if off >= c.size {
			return 0, io.EOF
		}
		p, eof = p[:c.size-off], io.EOF
	}
	for len(p) > 0 {
		n := min(len(p), maxIO)
		if err := c.read(p[:n], off); err != nil {
			return total, err
		}
		total, off, p = total+n, off+int64(n), p[n:]
	}
	return total, eof
}

func (c *Client) read(p []byte, off int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cookie, err := c.send(cmdRead, off, uint32(len(p)), nil)
	if err != nil {
		return err
	}
	// A server may leave ranges out only by reporting them as holes,
	// but clear the buffer anyway rather than trust that.
	clear(p)
	var h [12]byte
	return c.reply(cookie, func(typ uint16, n uint32) error {
		switch typ {
		case 0: // the data of a simple reply
			_, err := io.ReadFull(c.c, p)
			return err
		case chunkData:
			if n < 8 {
				return errors.New("short data chunk")
			}
			if _, err := io.ReadFull(c.c, h[:8]); err != nil {
				return err
			}
			at, l := int64(binary.BigEndian.Uint64(h[:]))-off, int64(n-8)
			if at < 0 || at+l > int64(len(p)) {
				return errors.New("data outside the range asked for")
			}
			_, err := io.ReadFull(c.c, p[at:at+l])
			return err
		case chunkHole:
			if n != 12 {
				return errors.New("malformed hole chunk")
			}
			if _, err := io.ReadFull(c.c, h[:12]); err != nil {
				return err
			}
			at, l := int64(binary.BigEndian.Uint64(h[:]))-off, int64(binary.BigEndian.Uint32(h[8:]))
			if at < 0 || at+l > int64(len(p)) {
				return errors.New("hole outside the range asked for")
			}
			clear(p[at : at+l])
			return nil
		}
		_, err := io.CopyN(io.Discard, c.c, int64(n))
		return err
	})
}

// WriteAt writes to the export.
func (c *Client) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > c.size {
		return 0, errors.New("nbd: write outside the disk")
	}
	total := 0
	for len(p) > 0 {
		n := min(len(p), maxIO)
		c.mu.Lock()
		cookie, err := c.send(cmdWrite, off, uint32(n), p[:n])
		if err == nil {
			err = c.reply(cookie, nil)
		}
		c.mu.Unlock()
		if err != nil {
			return total, err
		}
		total, off, p = total+n, off+int64(n), p[n:]
	}
	return total, nil
}

// Flush asks the server to make everything written durable.
func (c *Client) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flags&4 == 0 {
		return nil
	}
	cookie, err := c.send(cmdFlush, 0, 0, nil)
	if err != nil {
		return err
	}
	return c.reply(cookie, nil)
}

// Status returns the extents of a status context over the whole export,
// in ascending order and without gaps. Neighbours with equal flags are
// joined.
func (c *Client) Status(context string) ([]Extent, error) {
	id, ok := c.contexts[context]
	if !ok {
		return nil, fmt.Errorf("the server does not offer %q", context)
	}
	var out []Extent
	for off := int64(0); off < c.size; {
		got, err := c.status(id, off, min(c.size-off, maxStatus))
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			return nil, errors.New("nbd server: empty block status")
		}
		for _, e := range got {
			if e.Len <= 0 {
				return nil, errors.New("nbd server: empty extent in the block status")
			}
			e.Off = off
			if off+e.Len > c.size {
				e.Len = c.size - off
			}
			off += e.Len
			if n := len(out); n > 0 && out[n-1].Flags == e.Flags {
				out[n-1].Len += e.Len
			} else {
				out = append(out, e)
			}
			if off >= c.size {
				break
			}
		}
	}
	return out, nil
}

func (c *Client) status(id uint32, off, length int64) ([]Extent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cookie, err := c.send(cmdBlockStatus, off, uint32(length), nil)
	if err != nil {
		return nil, err
	}
	var out []Extent
	err = c.reply(cookie, func(typ uint16, n uint32) error {
		if typ != chunkStatus || n < 4 || (n-4)%8 != 0 || n > 64<<20 {
			_, err := io.CopyN(io.Discard, c.c, int64(n))
			return err
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(c.c, body); err != nil {
			return err
		}
		if binary.BigEndian.Uint32(body) != id {
			return nil
		}
		for b := body[4:]; len(b) >= 8; b = b[8:] {
			out = append(out, Extent{Len: int64(binary.BigEndian.Uint32(b)), Flags: binary.BigEndian.Uint32(b[4:])})
		}
		return nil
	})
	return out, err
}

// Close ends the session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		c.send(cmdDisc, 0, 0, nil)
		c.err = errors.New("nbd: connection closed")
	}
	return c.c.Close()
}
