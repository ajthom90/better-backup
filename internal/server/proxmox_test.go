package server_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// TestProxmox backs up and restores a machine the way it happens on a
// Proxmox VE node, without one. The machine is a real QEMU, set up as
// Proxmox sets its machines up, so reading its disks while it runs, the
// change tracking and the restored disks are the real thing. Proxmox's
// own tools (qm, pvesm, pvesh) are stood in for by testdata/fakepve, so
// what this cannot show is that the real tools answer as expected.
func TestProxmox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux")
	}
	for _, tool := range []string{"qemu-system-x86_64", "qemu-nbd", "qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	fake, err := filepath.Abs("testdata/fakepve")
	if err != nil {
		t.Fatal(err)
	}
	// A short path: the QMP socket's has to fit in a socket address.
	root, err := os.MkdirTemp("", "pve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("BB_PVE_ROOT", root)
	t.Setenv("BB_SCRATCH_DIR", t.TempDir())
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, d := range []string{"etc/pve/qemu-server", "var/run/qemu-server", "storage/local/images/101", "storage/tank"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	write(t, filepath.Join(root, "storages"), []byte("local dir\ntank zfspool\nold lvm\n"))
	os.MkdirAll(filepath.Join(root, "storage/old"), 0o755)

	disk := filepath.Join(root, "storage/local/images/101/vm-101-disk-0.qcow2")
	data := filepath.Join(root, "storage/tank/vm-101-disk-1")
	efi := filepath.Join(root, "storage/tank/vm-101-disk-0")
	skipped := filepath.Join(root, "storage/tank/vm-101-disk-2")
	sh(t, "qemu-img", "create", "-q", "-f", "qcow2", disk, fmt.Sprint(96<<20))
	sh(t, "qemu-io", "-f", "qcow2", "-c", "write -q -P 0x11 1M 3M", "-c", "write -q -P 0x22 70M 1500k", disk)
	sh(t, "qemu-img", "create", "-q", "-f", "raw", data, "8M")
	sh(t, "qemu-io", "-f", "raw", "-c", "write -q -P 0x33 2M 1M", data)
	sh(t, "qemu-img", "create", "-q", "-f", "raw", efi, "528K")
	sh(t, "qemu-io", "-f", "raw", "-c", "write -q -P 0x5a 0 528k", efi)
	sh(t, "qemu-img", "create", "-q", "-f", "raw", skipped, "4M")
	conf := filepath.Join(root, "etc/pve/qemu-server/101.conf")
	write(t, conf, []byte(`agent: 1
args: -no-shutdown
bios: ovmf
boot: order=scsi0;net0
cores: 2
description: The web server%0Asecond line, with a %25 sign
efidisk0: tank:vm-101-disk-0,efitype=4m,pre-enrolled-keys=1,size=528K
ide2: none,media=cdrom
memory: 2048
meta: creation-qemu=8.1.5,ctime=1700000000
name: web01
net0: virtio=BC:24:11:2A:3B:4C,bridge=vmbr0,firewall=1
scsi0: local:101/vm-101-disk-0.qcow2,discard=on,iothread=1,size=96M
scsi1: /dev/disk/by-id/ata-SOMETHING,size=1G
scsihw: virtio-scsi-single
smbios1: uuid=7d1e1b56-4e1c-4a55-9a0e-6c1f5f2a9b10
tpmstate0: tank:vm-101-disk-9,size=4M,version=v2.0
unused0: tank:vm-101-disk-7
virtio1: tank:vm-101-disk-1,size=8M
virtio2: tank:vm-101-disk-2,backup=0,size=4M
vmgenid: 0b1c2d3e-0000-4000-8000-000000000001

[before-upgrade]
cores: 1
scsi0: local:101/vm-101-disk-0.qcow2,size=1M
snaptime: 1700000001
`))
	// A second machine that shares nothing with the first.
	write(t, filepath.Join(root, "etc/pve/qemu-server/100.conf"), []byte("name: empty\nmemory: 512\nide2: none,media=cdrom\n"))

	qmpSocket := filepath.Join(root, "var/run/qemu-server/101.qmp")
	var qemu *exec.Cmd
	start := func() {
		t.Helper()
		qemu = exec.Command("qemu-system-x86_64", "-nodefaults", "-display", "none", "-S", "-m", "32",
			"-qmp", "unix:"+qmpSocket+",server=on,wait=off",
			"-drive", "file="+disk+",if=none,id=drive-scsi0,format=qcow2",
			"-device", "virtio-scsi-pci,id=scsihw0", "-device", "scsi-hd,drive=drive-scsi0,id=scsi0",
			"-drive", "file="+data+",if=none,id=drive-virtio1,format=raw",
			"-device", "virtio-blk-pci,drive=drive-virtio1,id=virtio1",
			"-drive", "file="+efi+",if=none,id=drive-efidisk0,format=raw",
			"-drive", "file="+skipped+",if=none,id=drive-virtio2,format=raw")
		var errb bytes.Buffer
		qemu.Stderr = &errb
		if err := qemu.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			if _, err := os.Stat(qmpSocket); err == nil {
				break
			}
			if i > 200 {
				t.Fatalf("QEMU did not start: %s", errb.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	stop := func() {
		if qemu != nil {
			qemu.Process.Kill()
			qemu.Wait()
			qemu = nil
			os.Remove(qmpSocket)
		}
	}
	t.Cleanup(stop)
	start()
	// guestWrite writes to a disk through the running QEMU, as the
	// guest would. socat is not needed: QEMU's own client does it.
	guestWrite := func(drive, pattern, at, length string) {
		t.Helper()
		script := fmt.Sprintf(`{"execute":"qmp_capabilities"}
{"execute":"human-monitor-command","arguments":{"command-line":"qemu-io %s \"write -P %s %s %s\""}}
`, drive, pattern, at, length)
		out := qmpTalk(t, qmpSocket, script)
		if strings.Contains(out, "rror") {
			t.Fatalf("guest write: %s", out)
		}
	}

	e, agentID := stack(t)
	var pol api.Policy
	// No hypervisor named: the agent has to find out it is on Proxmox.
	e.do("POST", "/api/v1/policies", api.Policy{Name: "vms", AgentID: agentID, VMs: []string{"web01"}, VMTracking: "on", Enabled: true}, &pol)
	check := func(j *api.Job, allow ...string) {
		t.Helper()
		if j.Status == api.StatusFailed {
			t.Fatalf("job %s failed: %s %v", j.ID, j.Error, j.Warnings)
		}
		seen := map[string]bool{}
	next:
		for _, w := range j.Warnings {
			for _, a := range allow {
				if strings.Contains(w, a) {
					seen[a] = true
					continue next
				}
			}
			t.Fatalf("unexpected warning: %s", w)
		}
		for _, a := range allow {
			if !seen[a] {
				t.Fatalf("missing the warning about %q in %v", a, j.Warnings)
			}
		}
	}
	backupNotes := []string{"TPM", "scsi1"}

	// 1. Running, first backup: everything that holds data.
	j, st := e.backup(pol.ID)
	check(j, backupNotes...)
	if st.BytesRead < 6<<20 || st.BytesRead > 9<<20 {
		t.Fatalf("first backup read %d bytes; the disks hold about 7 MiB", st.BytesRead)
	}
	// 2. Only what the guest wrote since, on the raw volume too.
	guestWrite("drive-scsi0", "0x44", "40M", "2M")
	guestWrite("drive-virtio1", "0x55", "5M", "1M")
	j, st = e.backup(pol.ID)
	check(j, backupNotes...)
	if st.BytesRead != 3<<20 {
		t.Fatalf("second backup read %d bytes, want the 3 MiB that changed", st.BytesRead)
	}
	// 3. Checking mode reads all and agrees with the tracking. "*"
	// also finds the machine without disks and says so.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{"*"}, Hypervisor: "proxmox", VMTracking: "check", Enabled: true}, &pol)
	guestWrite("drive-scsi0", "0x66", "1500k", "64k")
	j, st = e.backup(pol.ID)
	check(j, append(backupNotes, "empty was not backed up: it has no disks")...)
	if st.BytesRead < 9<<20 {
		t.Fatalf("checking backup read %d bytes", st.BytesRead)
	}
	live := j.SnapshotID

	// 4. Restore beside the original, onto the storages the disks were
	// on. The new machine gets the next free id.
	entry := "web01"
	restore := func(snapshot, target, newName string) *api.Job {
		t.Helper()
		var r api.Job
		e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: snapshot, Include: []string{entry}, Target: target, VMName: newName}, &r)
		return e.waitLong(r.ID)
	}
	restoreNotes := []string{"TPM", "scsi1", `"args"`, "virtio2"}
	r := restore(live, "", "web01-copy")
	check(r, restoreNotes...)
	got := readConf(t, filepath.Join(root, "etc/pve/qemu-server/102.conf"))
	for k, want := range map[string]string{
		"name": "web01-copy", "cores": "2", "bios": "ovmf", "boot": "order=scsi0;net0", "agent": "1",
		"scsi0":       "local:102/vm-102-disk-0.qcow2,discard=on,iothread=1,size=96M",
		"virtio1":     "tank:vm-102-disk-1,size=8M",
		"efidisk0":    "tank:vm-102-disk-0,efitype=4m,pre-enrolled-keys=1,size=528K",
		"ide2":        "none,media=cdrom",
		"scsi1":       "/dev/disk/by-id/ata-SOMETHING,size=1G",
		"net0":        "virtio=BC:24:11:2A:3B:4C,bridge=vmbr0,firewall=1",
		"description": `The web server\nsecond line, with a % sign`,
		"smbios1":     "uuid=7d1e1b56-4e1c-4a55-9a0e-6c1f5f2a9b10",
		"vmgenid":     "11111111-2222-3333-4444-555555555555",
	} {
		if got[k] != want {
			t.Errorf("restored %s = %q, want %q", k, got[k], want)
		}
	}
	for _, k := range []string{"args", "unused0", "tpmstate0", "meta", "virtio2", "snaptime", "lock"} {
		if v, ok := got[k]; ok {
			t.Errorf("restored machine has %s = %q", k, v)
		}
	}
	if t.Failed() {
		t.Fatalf("restored configuration: %v", got)
	}
	compare := func(id, storage string) {
		t.Helper()
		c := readConf(t, filepath.Join(root, "etc/pve/qemu-server", id+".conf"))
		for key, orig := range map[string]string{"scsi0": disk, "virtio1": data, "efidisk0": efi} {
			vol := strings.Split(c[key], ",")[0]
			if storage != "" && !strings.HasPrefix(vol, storage+":") {
				t.Fatalf("%s of machine %s is %s, not on %s", key, id, vol, storage)
			}
			path := sh(t, "pvesm", "path", vol)
			f1, f2 := "raw", "raw"
			if strings.HasSuffix(orig, ".qcow2") {
				f1 = "qcow2"
			}
			if strings.HasSuffix(path, ".qcow2") {
				f2 = "qcow2"
			}
			// -s: the restored volume may be rounded up to whole KiB.
			if out, err := exec.Command("qemu-img", "compare", "-U", "-f", f1, "-F", f2, orig, path).CombinedOutput(); err != nil {
				t.Fatalf("%s of machine %s differs from the original: %s", key, id, out)
			}
		}
	}
	compare("102", "")

	// 5. A name Proxmox does not accept fails before any disk is
	// written, and so does a storage that does not exist.
	if r := restore(live, "", "not a name"); r.Status != api.StatusFailed || !strings.Contains(r.Error, "DNS") {
		t.Fatalf("restore under a bad name: %s %s", r.Status, r.Error)
	}
	if r := restore(live, "nowhere", "web01-x"); r.Status != api.StatusFailed || !strings.Contains(r.Error, "nowhere") {
		t.Fatalf("restore to a missing storage: %s %s", r.Status, r.Error)
	}
	if r := restore(live, "/var/lib/vz", "web01-x"); r.Status != api.StatusFailed || !strings.Contains(r.Error, "name of a storage") {
		t.Fatalf("restore to a folder: %s %s", r.Status, r.Error)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/pve/qemu-server/103.conf")); err == nil {
		t.Fatal("a refused restore left a machine behind")
	}

	// 6. Switched off, the machine is read from its volumes.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{"101"}, VMTracking: "on", Enabled: true}, &pol)
	stop()
	sh(t, "qemu-io", "-f", "qcow2", "-c", "write -q -P 0x77 90M 1M", disk)
	j, st = e.backup(pol.ID)
	check(j, backupNotes...)
	if st.BytesRead < 10<<20 || st.BytesRead > 13<<20 {
		t.Fatalf("offline backup read %d bytes", st.BytesRead)
	}
	// 7. Running again, the record of changes is gone with the old
	// QEMU: one full read, then incremental again.
	start()
	j, st = e.backup(pol.ID)
	check(j, backupNotes...)
	if st.BytesRead < 10<<20 {
		t.Fatalf("backup after a restart read %d bytes; it has to read in full", st.BytesRead)
	}
	guestWrite("drive-scsi0", "0x88", "20M", "1M")
	j, st = e.backup(pol.ID)
	check(j, backupNotes...)
	if st.BytesRead != 1<<20 {
		t.Fatalf("last backup read %d bytes, want the 1 MiB that changed", st.BytesRead)
	}
	stop()

	// 8. The original is deleted. Restored onto one storage, the
	// machine gets its old id back.
	os.Remove(conf)
	entry = "101" // the policy now names the machine by its id
	r = restore(j.SnapshotID, "tank", "web01")
	check(r, restoreNotes...)
	compare("101", "tank")
	// 9. Plain LVM hands out volumes that hold old data (the stand-in
	// fills them with 0xEE), so there the holes are written as zeros.
	r = restore(j.SnapshotID, "old", "web01-on-lvm")
	check(r, restoreNotes...)
	compare("103", "old")
}

func readConf(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			m[k] = v
		}
	}
	return m
}
