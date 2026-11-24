package server_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

// stack starts a server and one enrolled agent and returns a client for
// the admin API and the agent's id.
func stack(t *testing.T) (*env, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	data := t.TempDir()
	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx); srv.Close(); close(done) }()
	base := "https://" + srv.Addr()
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}
	var tok api.EnrollToken
	e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, base, tok.Token, tok.Fingerprint, "vm-host"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(adir, log)
	if err != nil {
		t.Fatal(err)
	}
	agentDone := make(chan struct{})
	go func() { ag.Run(ctx); close(agentDone) }()
	t.Cleanup(func() { cancel(); <-agentDone; <-done })
	var agents []api.Agent
	e.do("GET", "/api/v1/agents", nil, &agents)
	return e, agents[0].ID
}

// waitLong is wait for jobs that read whole disks.
func (e *env) waitLong(id string) *api.Job {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var j api.Job
		e.do("GET", "/api/v1/jobs/"+id, nil, &j)
		if j.Status != api.StatusQueued && j.Status != api.StatusRunning {
			return &j
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("job %s did not finish", id)
	return nil
}

func (e *env) backup(policy string) (*api.Job, tree.Stats) {
	e.t.Helper()
	var j api.Job
	e.do("POST", "/api/v1/policies/"+policy+"/run", nil, &j)
	done := e.waitLong(j.ID)
	var st tree.Stats
	if len(done.Result) > 0 {
		if err := jsonUnmarshal(done.Result, &st); err != nil {
			e.t.Fatal(err)
		}
	}
	return done, st
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestLibvirt backs up and restores a real KVM machine through libvirt:
// in full, by change tracking, switched off, and as a new machine. It
// needs root, a running libvirtd and QEMU, so it only runs when
// BB_TEST_LIBVIRT is set to a folder QEMU can use for disk images, such
// as /var/lib/libvirt/images.
func TestLibvirt(t *testing.T) {
	images := os.Getenv("BB_TEST_LIBVIRT")
	if images == "" {
		t.Skip("BB_TEST_LIBVIRT is not set")
	}
	uri := os.Getenv("BB_LIBVIRT_URI")
	if uri == "" {
		uri = "qemu:///system"
	}
	virsh := func(args ...string) string {
		t.Helper()
		return sh(t, "virsh", append([]string{"-c", uri}, args...)...)
	}
	work, err := os.MkdirTemp(images, "bbtest-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(work, 0o755)
	name := "bbtest-" + filepath.Base(work)[7:]
	clone := name + "-restored"
	t.Cleanup(func() {
		all, _ := exec.Command("virsh", "-c", uri, "list", "--all", "--name").Output()
		for _, d := range strings.Split(string(all), "\n") {
			if !strings.HasPrefix(d, name) {
				continue
			}
			exec.Command("virsh", "-c", uri, "destroy", d).Run()
			exec.Command("virsh", "-c", uri, "undefine", d, "--nvram", "--checkpoints-metadata").Run()
		}
		os.RemoveAll(work)
	})

	// Two disks: a qcow2 one, which can be tracked, with data in two
	// places and nothing in between, and a small raw one.
	const size = 96 << 20
	disk, raw := filepath.Join(work, "disk.qcow2"), filepath.Join(work, "data.raw")
	sh(t, "qemu-img", "create", "-q", "-f", "qcow2", disk, fmt.Sprint(size))
	sh(t, "qemu-io", "-f", "qcow2", "-c", "write -q -P 0x11 1M 3M", "-c", "write -q -P 0x22 70M 1500k", disk)
	sh(t, "qemu-img", "create", "-q", "-f", "raw", raw, "8M")
	sh(t, "qemu-io", "-f", "raw", "-c", "write -q -P 0x33 2M 1M", raw)
	def := filepath.Join(work, "domain.xml")
	kind := "qemu"
	if _, err := os.Stat("/dev/kvm"); err == nil {
		kind = "kvm"
	}
	// With UEFI firmware installed the machine uses it, so that its
	// variable store is backed up and restored as well.
	osAttr := ""
	if m, _ := filepath.Glob("/usr/share/OVMF/OVMF_CODE*.fd"); len(m) > 0 {
		osAttr = " firmware='efi'"
	}
	write(t, def, []byte(`<domain type='`+kind+`'>
  <name>`+name+`</name>
  <title>a &lt;test&gt; machine</title>
  <memory unit='MiB'>64</memory>
  <vcpu>1</vcpu>
  <os`+osAttr+`><type arch='x86_64' machine='q35'>hvm</type></os>
  <features><acpi/></features>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' discard='unmap'/>
      <source file='`+disk+`'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='`+raw+`'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <interface type='user'><mac address='52:54:00:12:34:56'/><model type='virtio'/></interface>
  </devices>
</domain>`))
	virsh("define", def)
	virsh("start", name)

	// guestWrite writes to a disk the way the guest would, through the
	// running QEMU, so that its change tracking sees it.
	guestWrite := func(target, pattern, at, length string) {
		t.Helper()
		dev := "/machine/peripheral/" + map[string]string{"vda": "virtio-disk0", "vdb": "virtio-disk1"}[target] + "/virtio-backend"
		out := virsh("qemu-monitor-command", name, "--hmp", fmt.Sprintf(`qemu-io -d %s "write -P %s %s %s"`, dev, pattern, at, length))
		if out != "" {
			t.Fatalf("guest write: %s", out)
		}
	}

	// A backup job left behind by an agent that died is in the way of
	// the next one, which ends it.
	stray := filepath.Join(work, "stray.xml")
	sockets, _ := filepath.Glob("/var/lib/libvirt/qemu/domain-*-" + name)
	if len(sockets) != 1 {
		t.Fatalf("the machine's folder under libvirt's state: %v", sockets)
	}
	write(t, stray, []byte("<domainbackup mode='pull'><server transport='unix' socket='"+sockets[0]+"/bb-0000.sock'/></domainbackup>"))
	virsh("backup-begin", name, stray)

	e, agentID := stack(t)
	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "vms", AgentID: agentID, VMs: []string{name}, Hypervisor: "libvirt", VMTracking: "on", Enabled: true}, &pol)
	noWarnings := func(j *api.Job, allow ...string) {
		t.Helper()
		if j.Status != api.StatusSuccess && len(allow) == 0 || j.Status == api.StatusFailed {
			t.Fatalf("job %s: %s %s %v", j.ID, j.Status, j.Error, j.Warnings)
		}
	next:
		for _, w := range j.Warnings {
			for _, a := range allow {
				if strings.Contains(w, a) {
					continue next
				}
			}
			t.Fatalf("unexpected warning: %s", w)
		}
	}

	// 1. The first backup reads everything that holds data, and no more.
	j, st := e.backup(pol.ID)
	noWarnings(j)
	if st.BytesRead < 5<<20 || st.BytesRead > 9<<20 {
		t.Fatalf("first backup read %d bytes; the disks hold about 6 MiB of data", st.BytesRead)
	}
	first := virsh("checkpoint-list", name, "--name")
	if !strings.HasPrefix(first, "bb-"+pol.ID+"-") || strings.Contains(first, "\n") {
		t.Fatalf("checkpoints after the first backup: %q", first)
	}

	// 2. The guest writes 2 MiB to the tracked disk and 1 MiB to the raw
	// one. The next backup reads those blocks of the tracked disk, and
	// the raw disk's data in full.
	guestWrite("vda", "0x44", "40M", "2M")
	guestWrite("vdb", "0x55", "5M", "1M")
	j, st = e.backup(pol.ID)
	noWarnings(j)
	if st.BytesRead < 4<<20 || st.BytesRead > 5<<20 {
		// QEMU reports a block of a raw file as data that holds none.
		t.Fatalf("second backup read %d bytes, want the 2 MiB that changed and the raw disk's 2 MiB", st.BytesRead)
	}
	second := virsh("checkpoint-list", name, "--name")
	if second == first || strings.Count(second, "\n") != 1 || !strings.Contains(second, first) {
		// The first one stays until the second backup is known to be
		// the parent of the next.
		t.Fatalf("checkpoints after the second backup: %q (first %q)", second, first)
	}

	// 3. Checking mode reads everything and agrees with the tracking.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{"*"}, VMTracking: "check", Enabled: true}, &pol)
	guestWrite("vda", "0x66", "1500k", "64k")
	j, st = e.backup(pol.ID)
	for _, w := range j.Warnings {
		// "*" also finds whatever else is defined on this host.
		if strings.Contains(w, name) {
			t.Fatalf("unexpected warning: %s", w)
		}
	}
	if j.Status == api.StatusFailed || st.BytesRead < 8<<20 {
		t.Fatalf("checking backup: %s %s, read %d bytes", j.Status, j.Error, st.BytesRead)
	}
	if got := virsh("checkpoint-list", name, "--name"); strings.Count(got, "\n") != 1 || strings.Contains(got, first) {
		t.Fatalf("checkpoints after the third backup: %q", got)
	}
	live := j.SnapshotID

	// 4. Restore it beside the original, as a new machine, and compare
	// the disks with the originals.
	restore := func(snapshot, newName string) *api.Job {
		t.Helper()
		var r api.Job
		e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: snapshot, Include: []string{name}, Target: work, VMName: newName}, &r)
		return e.waitLong(r.ID)
	}
	// exact says the machine was off when backed up, so that even its
	// UEFI settings, which a running firmware keeps writing, must match.
	same := func(newName string, exact bool) {
		t.Helper()
		sh(t, "qemu-img", "compare", "-U", "-f", "qcow2", "-F", "qcow2", disk, filepath.Join(work, newName, "vda.qcow2"))
		sh(t, "qemu-img", "compare", "-U", "-f", "raw", "-F", "qcow2", raw, filepath.Join(work, newName, "vdb.qcow2"))
		xml := virsh("dumpxml", newName)
		for _, want := range []string{"<name>" + newName + "</name>", filepath.Join(work, newName, "vda.qcow2"), filepath.Join(work, newName, "vdb.qcow2"),
			"52:54:00:12:34:56", "a &lt;test&gt; machine", "discard='unmap'", "device='cdrom'"} {
			if !strings.Contains(xml, want) {
				t.Fatalf("the restored machine's definition lacks %q:\n%s", want, xml)
			}
		}
		if strings.Contains(xml, disk) || strings.Contains(xml, raw) {
			t.Fatalf("the restored machine still points at the original's disks:\n%s", xml)
		}
		if osAttr != "" {
			vars := filepath.Join(work, newName, newName+"_VARS.fd")
			if !strings.Contains(xml, vars) {
				t.Fatalf("the restored machine does not use its restored UEFI settings:\n%s", xml)
			}
			orig := strings.TrimSpace(sh(t, "sh", "-c", "virsh -c "+uri+" dumpxml "+name+" | sed -n 's/.*<nvram[^>]*>\\(.*\\)<.nvram>.*/\\1/p'"))
			a, err1 := os.ReadFile(orig)
			b, err2 := os.ReadFile(vars)
			if err1 != nil || err2 != nil || len(a) == 0 || len(a) != len(b) || exact && string(a) != string(b) {
				t.Fatalf("UEFI settings: original %s (%d bytes, %v), restored %d bytes (%v)", orig, len(a), err1, len(b), err2)
			}
		}
		if got := virsh("domstate", newName); got != "shut off" {
			t.Fatalf("the restored machine is %q", got)
		}
	}
	r := restore(live, clone)
	noWarnings(r, "the original machine still exists")
	if len(r.Warnings) != 1 {
		t.Fatalf("restore warnings: %v", r.Warnings)
	}
	same(clone, false)
	// The restored machine starts.
	virsh("start", clone)
	virsh("destroy", clone)
	// A second restore under the same name is refused before anything
	// is written.
	if r := restore(live, clone); r.Status != api.StatusFailed || !strings.Contains(r.Error, "already exists") {
		t.Fatalf("restore over an existing machine: %s %s", r.Status, r.Error)
	}

	// 5. Switched off, the machine is read from its files. Change
	// tracking does not apply, so everything that holds data is read.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{name}, VMTracking: "check", Enabled: true}, &pol)
	virsh("destroy", name)
	sh(t, "qemu-io", "-f", "qcow2", "-c", "write -q -P 0x77 90M 1M", disk)
	j, st = e.backup(pol.ID)
	noWarnings(j)
	if st.BytesRead < 8<<20 || st.BytesRead > 13<<20 {
		t.Fatalf("offline backup: %s %s, read %d bytes", j.Status, j.Error, st.BytesRead)
	}
	if r := restore(j.SnapshotID, clone+"2"); r.Status == api.StatusFailed {
		t.Fatalf("restore of the offline backup: %s", r.Error)
	}
	same(clone+"2", true)

	// 6. Running again, tracking starts over with a full read, and the
	// backup after that is incremental once more.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{name}, VMTracking: "on", Enabled: true}, &pol)
	virsh("start", name)
	j, st = e.backup(pol.ID)
	noWarnings(j)
	if st.BytesRead < 8<<20 {
		t.Fatalf("backup after the offline one read %d bytes; it has to read in full", st.BytesRead)
	}
	guestWrite("vda", "0x88", "20M", "1M")
	j, st = e.backup(pol.ID)
	noWarnings(j)
	if st.BytesRead < 3<<20 || st.BytesRead > 4<<20 {
		t.Fatalf("last backup read %d bytes, want the 1 MiB that changed and the raw disk's 2 MiB", st.BytesRead)
	}

	// A backup job that ends before the disks are read is noticed.
	drv := hypervisor.ByKind("libvirt")
	fz, err := drv.Freeze(context.Background(), name, hypervisor.Options{Tag: "check"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fz.Check(); err != nil {
		t.Fatalf("check of a healthy backup job: %v", err)
	}
	virsh("domjobabort", name)
	// libvirt forgets the job a moment after the command returns.
	for i := 0; fz.Check() == nil; i++ {
		if i == 50 {
			t.Fatal("a backup job that ended early went unnoticed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, d := range fz.Disks {
		d.Close()
	}
	fz.Release()

	// 7. With tracking switched off the checkpoints go away.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{name}, Enabled: true}, &pol)
	j, _ = e.backup(pol.ID)
	noWarnings(j)
	if got := virsh("checkpoint-list", name, "--name"); got != "" {
		t.Fatalf("checkpoints left after tracking was switched off: %q", got)
	}

	// 8. Restored over the original: the machine that is there, running
	// and with a checkpoint from change tracking, is switched off and
	// kept under another name, and the restored one has its name and
	// its files in a new folder beside the old ones.
	e.do("PUT", "/api/v1/policies/"+pol.ID, api.Policy{Name: "vms", AgentID: agentID, VMs: []string{name}, VMTracking: "on", Enabled: true}, &pol)
	j, _ = e.backup(pol.ID)
	noWarnings(j)
	sh(t, "cp", disk, filepath.Join(work, "before.qcow2"))
	guestWrite("vda", "0x99", "30M", "1M") // what the restore takes back
	var over api.Job
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j.SnapshotID, Include: []string{name}, VMReplace: true}, &over)
	r = e.waitLong(over.ID)
	if r.Status != api.StatusWarning || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "is kept as") {
		t.Fatalf("restore over the original: %s %s %v", r.Status, r.Error, r.Warnings)
	}
	var aside string
	for _, d := range strings.Split(virsh("list", "--all", "--name"), "\n") {
		if strings.HasPrefix(d, name+" (replaced ") {
			aside = d
		}
	}
	if aside == "" || !strings.Contains(r.Warnings[0], aside) {
		t.Fatalf("the machine that was there is not kept: %q, %v", virsh("list", "--all", "--name"), r.Warnings)
	}
	if got := virsh("domstate", aside); got != "shut off" {
		t.Fatalf("the machine that was set aside is %q", got)
	}
	if xml := virsh("dumpxml", aside); !strings.Contains(xml, disk) {
		t.Fatalf("the machine that was set aside lost its disks:\n%s", xml)
	}
	folders, _ := filepath.Glob(filepath.Join(work, name+" restored *"))
	if len(folders) != 1 {
		t.Fatalf("folders of the restored machine: %v", folders)
	}
	xml := virsh("dumpxml", name)
	if !strings.Contains(xml, filepath.Join(folders[0], "vda.qcow2")) || strings.Contains(xml, disk) {
		t.Fatalf("the restored machine's disks:\n%s", xml)
	}
	sh(t, "qemu-img", "compare", "-f", "qcow2", "-F", "qcow2", filepath.Join(work, "before.qcow2"), filepath.Join(folders[0], "vda.qcow2"))
	// It starts, and the next backup of it works.
	virsh("start", name)
	if j, _ = e.backup(pol.ID); j.Status == api.StatusFailed {
		t.Fatalf("backup of the restored machine: %s", j.Error)
	}
	// Asked to replace a machine that is not there, it is an ordinary
	// restore that says so.
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: j.SnapshotID, Include: []string{name}, Target: work, VMName: name + "-new", VMReplace: true}, &over)
	r = e.waitLong(over.ID)
	if r.Status != api.StatusWarning || !strings.Contains(strings.Join(r.Warnings, "\n"), "no machine of that name") {
		t.Fatalf("replacing a machine that is not there: %s %s %v", r.Status, r.Error, r.Warnings)
	}
	virsh("domstate", name+"-new")
}

// TestHypervisorPolicy checks that a policy names a known hypervisor.
func TestHypervisorPolicy(t *testing.T) {
	e, agentID := stack(t)
	err := e.admin.Do("POST", "/api/v1/policies", api.Policy{Name: "vms", AgentID: agentID, VMs: []string{"a"}, Hypervisor: "xen"}, nil)
	if err == nil || !strings.Contains(err.Error(), "libvirt") {
		t.Fatalf("unknown hypervisor: %v", err)
	}
	for _, kind := range append([]string{""}, api.Hypervisors...) {
		if kind == "vmware" {
			continue // needs a server as well: see TestVMware
		}
		var p api.Policy
		e.do("POST", "/api/v1/policies", api.Policy{Name: "vms " + kind, AgentID: agentID, VMs: []string{"a"}, Hypervisor: kind}, &p)
		if p.Hypervisor != kind {
			t.Fatalf("saved hypervisor %q, want %q", p.Hypervisor, kind)
		}
	}
}

// qmpTalk sends lines to a QMP socket and returns the answers.
func qmpTalk(t *testing.T, socket, script string) string {
	t.Helper()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(c)
	var out strings.Builder
	line, _ := r.ReadString('\n') // the greeting
	out.WriteString(line)
	for _, cmd := range strings.Split(strings.TrimSpace(script), "\n") {
		if _, err := c.Write([]byte(cmd + "\n")); err != nil {
			t.Fatal(err)
		}
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("qmp: %v after %s", err, out.String())
			}
			out.WriteString(line)
			if strings.Contains(line, `"return"`) || strings.Contains(line, `"error"`) {
				break
			}
		}
	}
	return out.String()
}
