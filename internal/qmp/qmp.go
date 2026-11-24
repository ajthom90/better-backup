// Package qmp talks to a running QEMU over its machine protocol, QMP:
// one JSON command per line, one JSON answer per line.
package qmp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Error is a command QEMU refused.
type Error struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *Error) Error() string { return e.Desc }

// NotFound reports whether err says QEMU does not know the command.
func NotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Class == "CommandNotFound"
}

// Client is a QMP session.
type Client struct {
	mu sync.Mutex
	c  net.Conn
	r  *bufio.Reader
	id int
}

type message struct {
	QMP    json.RawMessage `json:"QMP"`
	Return json.RawMessage `json:"return"`
	Error  *Error          `json:"error"`
	Event  string          `json:"event"`
	ID     int             `json:"id"`
}

// Dial connects to a QMP unix socket and completes the greeting.
func Dial(ctx context.Context, socket string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	c := &Client{c: conn, r: bufio.NewReaderSize(conn, 1<<16)}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	m, err := c.read()
	if err == nil && m.QMP == nil {
		err = errors.New("no QMP greeting")
	}
	if err == nil {
		err = c.Do(ctx, "qmp_capabilities", nil, nil)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp %s: %w", socket, err)
	}
	return c, nil
}

func (c *Client) read() (*message, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("unreadable answer: %w", err)
	}
	return &m, nil
}

// Do runs one command. args may be nil; the answer is decoded into out
// unless that is nil.
func (c *Client) Do(ctx context.Context, command string, args, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	req := map[string]any{"execute": command, "id": c.id}
	if args != nil {
		req["arguments"] = args
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.c.SetDeadline(deadline)
	if _, err := c.c.Write(append(b, '\n')); err != nil {
		return err
	}
	for {
		m, err := c.read()
		if err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
		if m.Event != "" || m.ID != c.id {
			continue // an event, or the answer to a command given up on
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %w", command, m.Error)
		}
		if out != nil {
			if err := json.Unmarshal(m.Return, out); err != nil {
				return fmt.Errorf("%s: unreadable answer: %w", command, err)
			}
		}
		return nil
	}
}

// Close ends the session.
func (c *Client) Close() error { return c.c.Close() }
