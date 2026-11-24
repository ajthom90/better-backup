package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/nbd"
	"github.com/ajthom90/better-backup/internal/qemuimg"
	"github.com/ajthom90/better-backup/internal/qmp"
)

// startQEMU runs a real QEMU with two disks named the way Proxmox names
// them. It needs no guest: the machine sits at its firmware and the
// tests write to its disks through the monitor.
func startQEMU(t *testing.T, dir string) (qmpSocket, disk, raw string) {
	t.Helper()
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skip("qemu-system-x86_64 is not installed")
	}
	if err := qemuimg.Available(); err != nil {
		t.Skip(err)
	}
	disk, raw = filepath.Join(dir, "disk.qcow2"), filepath.Join(dir, "data.raw")
	sh(t, "qemu-img", "create", "-q", "-f", "qcow2", disk, fmt.Sprint(96<<20))
	sh(t, "qemu-io", "-f", "qcow2", "-c", "write -q -P 0x11 1M 3M", "-c", "write -q -P 0x22 70M 1500k", disk)
	sh(t, "qemu-img", "create", "-q", "-f", "raw", raw, "8M")
	sh(t, "qemu-io", "-f", "raw", "-c", "write -q -P 0x33 2M 1M", raw)
	qmpSocket = filepath.Join(dir, "qmp.sock")
	cmd := exec.Command("qemu-system-x86_64", "-nodefaults", "-display", "none", "-S", "-m", "32",
		"-qmp", "unix:"+qmpSocket+",server=on,wait=off",
		"-drive", "file="+disk+",if=none,id=drive-scsi0,format=qcow2",
		"-device", "virtio-scsi-pci,id=scsihw0", "-device", "scsi-hd,drive=drive-scsi0,id=scsi0",
		"-drive", "file="+raw+",if=none,id=drive-virtio1,format=raw",
		"-device", "virtio-blk-pci,drive=drive-virtio1,id=virtio1")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; ; i++ {
		if _, err := os.Stat(qmpSocket); err == nil {
			break
		}
		if i > 200 {
			t.Fatalf("QEMU did not start: %s", errb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return qmpSocket, disk, raw
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// guestWrite writes to a disk through the running QEMU, as the guest
// would.
func guestWrite(t *testing.T, qmpSocket, drive, pattern, at, length string) {
	t.Helper()
	q, err := qmp.Dial(context.Background(), qmpSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var out string
	cmd := fmt.Sprintf(`qemu-io %s "write -P %s %s %s"`, drive, pattern, at, length)
	if err := q.Do(context.Background(), "human-monitor-command", map[string]any{"command-line": cmd}, &out); err != nil || strings.Contains(out, "rror") {
		t.Fatalf("guest write: %v %s", err, out)
	}
}

// read returns a whole export and the bytes its context marks.
func read(t *testing.T, l *live, d liveDisk) (data []byte, changed int64) {
	t.Helper()
	contexts := []string{nbd.Allocation}
	if d.Changes != "" {
		contexts = append(contexts, nbd.DirtyBitmap(d.Changes))
	}
	c, err := nbd.Dial("unix", l.Socket, d.Key, contexts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data = make([]byte, c.Size())
	if _, err := c.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	changed = -1
	if d.Changes != "" {
		ext, err := c.Status(nbd.DirtyBitmap(d.Changes))
		if err != nil {
			t.Fatal(err)
		}
		changed = 0
		for _, e := range ext {
			if e.Flags&1 != 0 {
				changed += e.Len
				if !bytes.Equal(data[e.Off:e.Off+e.Len], bytes.Repeat([]byte{0x44}, int(e.Len))) && !bytes.Equal(data[e.Off:e.Off+e.Len], bytes.Repeat([]byte{0x66}, int(e.Len))) {
					t.Errorf("the changes of %s include a range nothing wrote to: %+v", d.Key, e)
				}
			}
		}
	}
	return data, changed
}

func count(b []byte, v byte) int { return bytes.Count(b, []byte{v}) }

// TestLive freezes the disks of a real QEMU the way a Proxmox backup
// does: the frozen view holds while the machine writes, bitmaps report
// exactly what was written between backups, a failed backup does not
// lose the record, and nothing is left behind in the QEMU.
func TestLive(t *testing.T) {
	dir := t.TempDir()
	sock, _, _ := startQEMU(t, dir)
	ctx := context.Background()
	scratch := filepath.Join(dir, "scratch")
	os.Mkdir(scratch, 0o700)
	want := []string{"scsi0", "virtio1"}

	// 1. A first backup, starting to track.
	l, err := startLive(ctx, sock, want, scratch, "bb-p-", "bb-p-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Disks) != 2 || l.Disks[0].Size != 96<<20 || l.Disks[1].Size != 8<<20 || l.Disks[0].Changes != "" {
		t.Fatalf("disks: %+v", l.Disks)
	}
	// The machine overwrites data while the backup reads: the view
	// must still show the old contents.
	guestWrite(t, sock, "drive-scsi0", "0x44", "2M", "1M")
	guestWrite(t, sock, "drive-scsi0", "0x44", "40M", "2M")
	guestWrite(t, sock, "drive-virtio1", "0x44", "5M", "1M")
	data, _ := read(t, l, l.Disks[0])
	if count(data, 0x11) != 3<<20 || count(data, 0x22) != 1500<<10 || count(data, 0x44) != 0 {
		t.Fatalf("the frozen disk shows what was written after the freeze: %d %d %d", count(data, 0x11), count(data, 0x22), count(data, 0x44))
	}
	if data, _ := read(t, l, l.Disks[1]); count(data, 0x33) != 1<<20 || count(data, 0x44) != 0 {
		t.Fatal("the frozen raw disk shows what was written after the freeze")
	}
	if err := l.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. The next backup sees exactly those writes as changes, on both
	// disks: bitmaps live in QEMU's memory, so raw disks have them too.
	l, err = startLive(ctx, sock, want, scratch, "bb-p-", "bb-p-2", map[string]string{"scsi0": "bb-p-1", "virtio1": "bb-p-1"})
	if err != nil {
		t.Fatal(err)
	}
	data, changed := read(t, l, l.Disks[0])
	if changed != 3<<20 || count(data, 0x44) != 3<<20 || count(data, 0x11) != 2<<20 {
		t.Fatalf("second backup: %d bytes changed, %d bytes of the new pattern", changed, count(data, 0x44))
	}
	if _, changed := read(t, l, l.Disks[1]); changed != 1<<20 {
		t.Fatalf("second backup of the raw disk: %d bytes changed", changed)
	}
	// 3. This backup is abandoned, as when the upload fails: the next
	// one starts from the first again and must see everything since.
	guestWrite(t, sock, "drive-scsi0", "0x66", "60M", "1M")
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	l, err = startLive(ctx, sock, want, scratch, "bb-p-", "bb-p-3", map[string]string{"scsi0": "bb-p-1", "virtio1": "bb-p-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := read(t, l, l.Disks[0]); changed != 4<<20 {
		t.Fatalf("backup after an abandoned one: %d bytes changed, want 4 MiB", changed)
	}

	// 4. The agent dies here. The next backup clears up after it and
	// still starts from the first.
	l2, err := startLive(ctx, sock, want, scratch, "bb-p-", "bb-p-4", map[string]string{"scsi0": "bb-p-1", "virtio1": "bb-p-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Check(ctx); err == nil {
		t.Fatal("a backup whose jobs were taken away still passes its check")
	}
	os.RemoveAll(l.dir)
	if _, changed := read(t, l2, l2.Disks[0]); changed != 4<<20 {
		t.Fatalf("backup after a dead one: %d bytes changed, want 4 MiB", changed)
	}
	// A bitmap that is gone, as after the machine was restarted, means
	// a full read, without an error.
	if err := l2.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	l, err = startLive(ctx, sock, want, scratch, "bb-p-", "bb-p-5", map[string]string{"scsi0": "bb-p-2", "virtio1": "bb-p-4"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Disks[0].Changes != "" || l.Disks[1].Changes == "" {
		t.Fatalf("bases: %+v", l.Disks)
	}
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// 5. With tracking off, everything this program added goes away.
	l, err = startLive(ctx, sock, want, scratch, "bb-p-", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	q, err := qmp.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	bs, err := blocks(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if len(b.Bitmaps) != 0 {
			t.Errorf("bitmaps left on %s: %v", b.Key, b.Bitmaps)
		}
	}
	var nodes []struct {
		NodeName string `json:"node-name"`
	}
	q.Do(ctx, "query-named-block-nodes", map[string]any{"flat": true}, &nodes)
	for _, n := range nodes {
		if strings.HasPrefix(n.NodeName, "bb-") {
			t.Errorf("node left behind: %s", n.NodeName)
		}
	}
	if js, _ := jobs(ctx, q); len(js) != 0 {
		t.Errorf("jobs left behind: %v", js)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("scratch files left behind: %v", left)
	}
}
