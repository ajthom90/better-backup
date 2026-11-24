// Package qemuimg reads and writes disk images in QEMU's formats by
// running qemu-nbd and qemu-img, which every QEMU host has. qemu-nbd
// serves an image's contents over a socket, whatever its format, and
// package nbd reads and writes them there.
package qemuimg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/nbd"
)

// Available returns nil when qemu-nbd and qemu-img are installed.
func Available() error {
	for _, tool := range []string{"qemu-nbd", "qemu-img"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not installed (it is part of the package qemu-utils or qemu-img)", tool)
		}
	}
	return nil
}

// Create makes an empty image.
func Create(ctx context.Context, path, format string, size int64) error {
	out, err := exec.CommandContext(ctx, "qemu-img", "create", "-q", "-f", format, path, strconv.FormatInt(size, 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating %s: %s", path, firstLine(out, err))
	}
	return nil
}

// Image is an image being served by qemu-nbd.
type Image struct {
	*nbd.Client
	cmd  *exec.Cmd
	dir  string
	done chan error
	errb *bytes.Buffer
}

// Open serves an image with qemu-nbd and connects to it. The format is
// always given, never guessed from the image's contents. A read-only
// image can be opened while nothing else writes to it; QEMU's own
// locking refuses if a running machine has it open.
func Open(ctx context.Context, path, format string, write bool, contexts ...string) (*Image, error) {
	dir, err := os.MkdirTemp("", "bb-nbd-")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "nbd.sock")
	args := []string{"--socket=" + sock, "--format=" + format}
	if !write {
		args = append(args, "--read-only")
	} else {
		args = append(args, "--discard=unmap")
	}
	// The path is not an option even if it starts with a dash.
	args = append(args, "--", path)
	// qemu-nbd is not tied to ctx: it must outlive a cancelled job long
	// enough to close the image cleanly, which Close sees to.
	im := &Image{cmd: exec.Command("qemu-nbd", args...), dir: dir, done: make(chan error, 1), errb: new(bytes.Buffer)}
	im.cmd.Stderr = im.errb
	if err := im.cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("starting qemu-nbd: %w", err)
	}
	go func() { im.done <- im.cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case err := <-im.done:
			os.RemoveAll(dir)
			return nil, fmt.Errorf("opening %s: %s", path, firstLine(im.errb.Bytes(), err))
		case <-ctx.Done():
			im.stop()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			im.stop()
			return nil, fmt.Errorf("opening %s: qemu-nbd did not start serving it", path)
		}
	}
	if im.Client, err = nbd.Dial("unix", sock, "", contexts...); err != nil {
		im.stop()
		return nil, err
	}
	return im, nil
}

func (im *Image) stop() {
	im.cmd.Process.Kill()
	<-im.done
	os.RemoveAll(im.dir)
}

// Close flushes what was written, ends the session and waits for
// qemu-nbd to close the image.
func (im *Image) Close() error {
	err := im.Flush()
	im.Client.Close()
	// qemu-nbd leaves when its only client does.
	select {
	case werr := <-im.done:
		if werr != nil && err == nil {
			err = errors.New(firstLine(im.errb.Bytes(), werr))
		}
	case <-time.After(60 * time.Second):
		im.cmd.Process.Kill()
		<-im.done
		if err == nil {
			err = errors.New("qemu-nbd did not close the image in time")
		}
	}
	os.RemoveAll(im.dir)
	return err
}

func firstLine(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return msg
}
