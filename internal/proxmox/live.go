package proxmox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/qemuimg"
	"github.com/ajthom90/better-backup/internal/qmp"
)

// This file reads the disks of a running QEMU through its machine
// protocol, with the scheme QEMU's documentation calls image fleecing.
// For each disk an empty scratch image is put on top of the disk, and a
// backup job with sync=none copies into it the old contents of whatever
// the machine is about to overwrite. Reading the scratch image then
// gives the disk as it was when the job began. QEMU's built-in NBD
// server exports the scratch images, and dirty bitmaps record what is
// written from one backup to the next.

// Everything this program adds to a QEMU carries one of these prefixes,
// so that what an interrupted backup left behind can be found.
const (
	scratchPrefix = "bb-scratch-"
	jobPrefix     = "bb-job-"
	exportPrefix  = "bb-export-"
	tmpPrefix     = "bbtmp-"
)

// block is one of a running machine's disks as QEMU sees it.
type block struct {
	Key     string // "scsi0"
	Node    string // QEMU's name for the disk's top node
	Size    int64
	Bitmaps map[string]bool // the usable dirty bitmaps on the node
}

type blockInfo struct {
	Device   string `json:"device"`
	Inserted *struct {
		NodeName string `json:"node-name"`
		Image    struct {
			VirtualSize int64 `json:"virtual-size"`
		} `json:"image"`
		DirtyBitmaps []bitmapInfo `json:"dirty-bitmaps"`
	} `json:"inserted"`
	DirtyBitmaps []bitmapInfo `json:"dirty-bitmaps"` // QEMU before 6.0
}

type bitmapInfo struct {
	Name         string `json:"name"`
	Inconsistent bool   `json:"inconsistent"`
	Recording    *bool  `json:"recording"`
}

// blocks lists the machine's disks by the key Proxmox gives them.
// Proxmox names a drive's device, or with newer versions its top node,
// "drive-" plus the key.
func blocks(ctx context.Context, q *qmp.Client) (map[string]block, error) {
	var infos []blockInfo
	if err := q.Do(ctx, "query-block", nil, &infos); err != nil {
		return nil, err
	}
	out := map[string]block{}
	for _, in := range infos {
		if in.Inserted == nil || in.Inserted.NodeName == "" {
			continue
		}
		name := in.Device
		if name == "" {
			name = in.Inserted.NodeName
		}
		key, ok := strings.CutPrefix(name, "drive-")
		if !ok {
			continue
		}
		b := block{Key: key, Node: in.Inserted.NodeName, Size: in.Inserted.Image.VirtualSize, Bitmaps: map[string]bool{}}
		for _, bm := range append(in.Inserted.DirtyBitmaps, in.DirtyBitmaps...) {
			if !bm.Inconsistent && (bm.Recording == nil || *bm.Recording || strings.HasPrefix(bm.Name, tmpPrefix)) {
				b.Bitmaps[bm.Name] = true
			}
		}
		out[key] = b
	}
	return out, nil
}

type jobInfo struct {
	Device   string `json:"device"`
	Status   string `json:"status"`
	IOStatus string `json:"io-status"`
}

func jobs(ctx context.Context, q *qmp.Client) (map[string]jobInfo, error) {
	var list []jobInfo
	if err := q.Do(ctx, "query-block-jobs", nil, &list); err != nil {
		return nil, err
	}
	out := map[string]jobInfo{}
	for _, j := range list {
		if strings.HasPrefix(j.Device, jobPrefix) {
			out[j.Device] = j
		}
	}
	return out, nil
}

// sweep removes what a backup of this program left in a QEMU: its
// exports, jobs, scratch nodes and temporary bitmaps. keep names
// tracking bitmaps to leave; every other bitmap starting with prefix
// goes too. It is used to end a backup and to clear up after one that
// was interrupted.
func sweep(ctx context.Context, q *qmp.Client, prefix string, keep map[string]bool) error {
	var problems []error
	running, err := jobs(ctx, q)
	if err != nil {
		return err
	}
	var nodes []struct {
		NodeName string `json:"node-name"`
	}
	if err := q.Do(ctx, "query-named-block-nodes", map[string]any{"flat": true}, &nodes); err != nil {
		// QEMU before 5.0 does not know "flat".
		if err := q.Do(ctx, "query-named-block-nodes", nil, &nodes); err != nil {
			return err
		}
	}
	var scratch []string
	for _, n := range nodes {
		if strings.HasPrefix(n.NodeName, scratchPrefix) {
			scratch = append(scratch, n.NodeName)
		}
	}
	if len(running) > 0 || len(scratch) > 0 {
		// The NBD server is this program's if its scratch nodes are
		// there to be exported. Stopping it also removes the exports.
		q.Do(ctx, "nbd-server-stop", nil, nil)
	}
	for id := range running {
		q.Do(ctx, "block-job-cancel", map[string]any{"device": id, "force": true}, nil)
	}
	for deadline := time.Now().Add(60 * time.Second); len(running) > 0; {
		if running, err = jobs(ctx, q); err != nil {
			return err
		}
		if len(running) == 0 {
			break
		}
		if time.Now().After(deadline) {
			problems = append(problems, errors.New("its backup jobs did not end"))
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, n := range scratch {
		// The node may be in use for a moment after its job ended.
		var derr error
		for try := 0; try < 20; try++ {
			if derr = q.Do(ctx, "blockdev-del", map[string]any{"node-name": n}, nil); derr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if derr != nil {
			problems = append(problems, derr)
		}
	}
	bs, err := blocks(ctx, q)
	if err != nil {
		return err
	}
	for _, b := range bs {
		for name := range b.Bitmaps {
			if strings.HasPrefix(name, tmpPrefix) || prefix != "" && strings.HasPrefix(name, prefix) && !keep[name] {
				if err := q.Do(ctx, "block-dirty-bitmap-remove", map[string]any{"node": b.Node, "name": name}, nil); err != nil {
					problems = append(problems, err)
				}
			}
		}
	}
	return errors.Join(problems...)
}

// liveDisk is one disk of a live backup.
type liveDisk struct {
	block
	// Base is the bitmap that recorded what was written since the
	// previous backup, or "" to read the disk in full.
	Base string
	// Changes is the name of the NBD context that lists those writes.
	Changes string
	// Job is the id of the backup job that holds the disk still.
	Job string
}

// live is a running machine's disks held still.
type live struct {
	qmpSocket string
	dir       string // scratch images and the NBD socket
	Socket    string // where the disks are served
	Disks     []liveDisk
	// Bitmap is the tracking bitmap this backup started on every disk,
	// or "".
	Bitmap string
	prefix string
	keep   map[string]bool
}

// dialQMP connects to a machine's QMP socket. Proxmox's own services
// use the same socket, which serves one client at a time, so sessions
// are kept short and a busy socket is waited for.
func dialQMP(ctx context.Context, socket string) (*qmp.Client, error) {
	var err error
	for try := 0; try < 4; try++ {
		var q *qmp.Client
		if q, err = qmp.Dial(ctx, socket); err == nil {
			return q, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, err
}

// startLive freezes the disks named by want (drive keys) of the QEMU
// behind a QMP socket. newBitmap, if not empty, starts change tracking
// under that name; base maps drive keys to the bitmap the previous
// backup started. Bitmaps of earlier backups that begin with prefix are
// removed, except those in base: if this backup fails, the next one
// still needs them.
func startLive(ctx context.Context, qmpSocket string, want []string, scratchDir, prefix, newBitmap string, base map[string]string) (l *live, err error) {
	q, err := dialQMP(ctx, qmpSocket)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	// Clear up after a backup that was interrupted.
	if err := sweep(ctx, q, "", nil); err != nil {
		return nil, fmt.Errorf("clearing up after an interrupted backup: %w", err)
	}
	bs, err := blocks(ctx, q)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(scratchDir, "bb-live-")
	if err != nil {
		return nil, err
	}
	l = &live{qmpSocket: qmpSocket, dir: dir, Socket: filepath.Join(dir, "nbd.sock"), Bitmap: newBitmap, prefix: prefix, keep: map[string]bool{}}
	if newBitmap != "" {
		l.keep[newBitmap] = true
	}
	defer func() {
		if err != nil {
			sweep(context.WithoutCancel(ctx), q, "", nil)
			if newBitmap != "" {
				for _, d := range l.Disks {
					q.Do(context.WithoutCancel(ctx), "block-dirty-bitmap-remove", map[string]any{"node": d.Node, "name": newBitmap}, nil)
				}
			}
			os.RemoveAll(dir)
			l = nil
		}
	}()
	tag := keys.RandomHex(4)
	var actions []map[string]any
	act := func(typ string, data map[string]any) {
		actions = append(actions, map[string]any{"type": typ, "data": data})
	}
	for _, key := range want {
		b, ok := bs[key]
		if !ok {
			return nil, fmt.Errorf("the running machine has no disk %s; if it was just added, the machine may need a restart", key)
		}
		d := liveDisk{block: b, Job: jobPrefix + tag + "-" + key}
		if name := base[key]; name != "" && b.Bitmaps[name] {
			d.Base = name
			l.keep[name] = true
		}
		scratch := filepath.Join(dir, key+".qcow2")
		if err := qemuimg.Create(ctx, scratch, "qcow2", b.Size); err != nil {
			return nil, err
		}
		err := q.Do(ctx, "blockdev-add", map[string]any{"driver": "qcow2", "node-name": scratchPrefix + key,
			"file": map[string]any{"driver": "file", "filename": scratch}, "backing": b.Node}, nil)
		if err != nil {
			return nil, fmt.Errorf("adding a scratch image for %s: %w", key, err)
		}
		if newBitmap != "" {
			act("block-dirty-bitmap-add", map[string]any{"node": b.Node, "name": newBitmap})
		}
		if d.Base != "" {
			// A copy of the record of changes, frozen at this moment;
			// the original keeps recording.
			d.Changes = tmpPrefix + tag + "-" + key
			act("block-dirty-bitmap-add", map[string]any{"node": b.Node, "name": d.Changes, "disabled": true})
			act("block-dirty-bitmap-merge", map[string]any{"node": b.Node, "target": d.Changes, "bitmaps": []string{d.Base}})
		}
		act("blockdev-backup", map[string]any{"job-id": d.Job, "device": b.Node, "target": scratchPrefix + key, "sync": "none"})
		l.Disks = append(l.Disks, d)
	}
	// One transaction: every disk is frozen at the same instant, and
	// the bitmaps change hands at exactly that instant.
	if err := q.Do(ctx, "transaction", map[string]any{"actions": actions}, nil); err != nil {
		return nil, fmt.Errorf("starting the backup jobs: %w", err)
	}
	err = q.Do(ctx, "nbd-server-start", map[string]any{"addr": map[string]any{"type": "unix", "data": map[string]any{"path": l.Socket}}}, nil)
	if err != nil {
		return nil, fmt.Errorf("starting QEMU's NBD server (is a migration or another backup using it?): %w", err)
	}
	for _, d := range l.Disks {
		args := map[string]any{"type": "nbd", "id": exportPrefix + d.Key, "node-name": scratchPrefix + d.Key, "name": d.Key}
		if d.Changes != "" {
			args["bitmaps"] = []string{d.Changes}
		}
		err := q.Do(ctx, "block-export-add", args, nil)
		if qmp.NotFound(err) {
			// QEMU before 5.2.
			old := map[string]any{"device": scratchPrefix + d.Key, "name": d.Key}
			if d.Changes != "" {
				old["bitmap"] = d.Changes
			}
			err = q.Do(ctx, "nbd-server-add", old, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("exporting the disk %s: %w", d.Key, err)
		}
	}
	// Bitmaps of backups before the previous one have done their work.
	if prefix != "" {
		for _, d := range l.Disks {
			for name := range d.Bitmaps {
				if strings.HasPrefix(name, prefix) && !l.keep[name] {
					q.Do(ctx, "block-dirty-bitmap-remove", map[string]any{"node": d.Node, "name": name}, nil)
				}
			}
		}
	}
	return l, nil
}

// Check returns an error if a backup job is no longer running. QEMU
// ends one when it cannot save what the machine overwrites, such as
// when the scratch folder is full, and what is read after that is not
// the disk as it was.
func (l *live) Check(ctx context.Context) error {
	q, err := dialQMP(ctx, l.qmpSocket)
	if err != nil {
		return fmt.Errorf("the machine cannot be asked whether its backup jobs held (was it stopped during the backup?): %w", err)
	}
	defer q.Close()
	running, err := jobs(ctx, q)
	if err != nil {
		return err
	}
	for _, d := range l.Disks {
		j, ok := running[d.Job]
		if !ok || j.IOStatus != "" && j.IOStatus != "ok" || j.Status == "aborting" || j.Status == "concluded" || j.Status == "null" {
			return fmt.Errorf("the backup job of its disk %s ended before the disk was read, so what was read may mix old and new contents (is %s full?)", d.Key, filepath.Dir(l.dir))
		}
	}
	return nil
}

// Stop ends the backup: the exports, jobs and scratch images go, the
// tracking bitmaps stay.
func (l *live) Stop(ctx context.Context) error {
	defer os.RemoveAll(l.dir)
	q, err := dialQMP(ctx, l.qmpSocket)
	if err != nil {
		if _, serr := os.Stat(l.qmpSocket); errors.Is(serr, os.ErrNotExist) {
			return nil // the machine was stopped; nothing is left of it
		}
		return err
	}
	defer q.Close()
	return sweep(ctx, q, l.prefix, l.keep)
}
