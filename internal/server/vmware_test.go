package server_test

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/tree"
	"github.com/ajthom90/better-backup/internal/vmware/vmwaretest"
)

// TestVMware backs up and restores machines of a simulated vCenter
// Server through the server, an agent and the admin API. The simulator
// is govmomi's; see package vmwaretest for what it is given to make
// that possible. Nothing here has met a real ESXi host.
func TestVMware(t *testing.T) {
	sim := vmwaretest.Start(t)
	e, agentID := stack(t)
	disk := func(seed int64, blocks int) []byte {
		b := make([]byte, blocks*vmwaretest.Block)
		rnd := rand.New(rand.NewSource(seed))
		for i := 0; i < blocks; i++ {
			if i%4 != 2 {
				rnd.Read(b[i*vmwaretest.Block : (i+1)*vmwaretest.Block])
			}
		}
		return b
	}
	first, second := disk(1, 6), disk(2, 4)
	sim.Plant(t, "DC0_H0_VM0", first)
	sim.Plant(t, "DC0_H0_VM1", second, disk(3, 2))

	// The server list: validated, and never showing a password.
	const password = "s3cret-vmware-pw"
	err := e.admin.Do("POST", "/api/v1/vmware-servers", api.VMwareServer{Name: "lab", URL: sim.Addr, Username: sim.Username}, nil)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("server without a password: %v", err)
	}
	err = e.admin.Do("POST", "/api/v1/vmware-servers", api.VMwareServer{Name: "lab", URL: sim.Addr, Username: sim.Username, Password: password, Thumbprint: "12:34"}, nil)
	if err == nil || !strings.Contains(err.Error(), "thumbprint") {
		t.Fatalf("server with a broken thumbprint: %v", err)
	}
	var vs api.VMwareServer
	e.do("POST", "/api/v1/vmware-servers", api.VMwareServer{Name: "lab", URL: sim.Addr, Username: sim.Username, Password: password}, &vs)
	if vs.ID == "" || vs.Password != "" || !vs.PasswordSet {
		t.Fatalf("saved server: %+v", vs)
	}
	noPassword := func(what string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if bytes.Contains(b, []byte(password)) {
			t.Fatalf("%s shows the VMware password", what)
		}
	}

	// Policies must name a server and cannot ask for change tracking.
	err = e.admin.Do("POST", "/api/v1/policies", api.Policy{Name: "vmw", AgentID: agentID, VMs: []string{"DC0_H0_VM0"}, Hypervisor: "vmware"}, nil)
	if err == nil || !strings.Contains(err.Error(), "VMware server") {
		t.Fatalf("policy without a server: %v", err)
	}
	err = e.admin.Do("POST", "/api/v1/policies", api.Policy{Name: "vmw", AgentID: agentID, VMs: []string{"DC0_H0_VM0"}, Hypervisor: "vmware", VMwareServer: vs.ID, VMTracking: "on"}, nil)
	if err == nil || !strings.Contains(err.Error(), "read in full") {
		t.Fatalf("policy with change tracking: %v", err)
	}
	var p api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "vmw", AgentID: agentID, VMs: []string{"DC0_H0_VM0"}, Hypervisor: "vmware", VMwareServer: vs.ID, Enabled: false}, &p)

	// No certificate is trusted yet: the job says which one the server
	// shows, and that is what gets pinned.
	j, _ := e.backup(p.ID)
	all := j.Error + strings.Join(j.Warnings, "\n")
	if !strings.Contains(strings.ReplaceAll(all, ":", ""), sim.Thumbprint) {
		t.Fatalf("backup without a trusted certificate: %s %q %v", j.Status, j.Error, j.Warnings)
	}
	e.do("PUT", "/api/v1/vmware-servers/"+vs.ID, api.VMwareServer{Name: "lab", URL: sim.Addr, Username: sim.Username, PasswordSet: true, Thumbprint: sim.Thumbprint}, &vs)
	if !vs.PasswordSet {
		t.Fatal("editing a server lost its password")
	}

	j, st := e.backup(p.ID)
	if j.Status != api.StatusSuccess || len(j.Warnings) != 0 {
		t.Fatalf("backup: %s %q %v", j.Status, j.Error, j.Warnings)
	}
	if st.BytesUploaded < 4*vmwaretest.Block {
		t.Fatalf("the first backup stored only %d bytes", st.BytesUploaded)
	}
	if got := sim.Snapshots("DC0_H0_VM0"); len(got) != 0 {
		t.Fatalf("snapshots left on the machine: %v", got)
	}
	// Nothing changed: the second backup reads it all and stores nothing.
	j, st = e.backup(p.ID)
	if j.Error != "" || st.BytesUploaded > vmwaretest.Block/2 {
		t.Fatalf("second backup: %q, %d bytes stored", j.Error, st.BytesUploaded)
	}

	// Every machine: the two with real disks are backed up, the rest are
	// left out with a warning each, and no snapshot stays behind.
	var every api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "all", AgentID: agentID, VMs: []string{"*"}, Hypervisor: "vmware", VMwareServer: vs.ID}, &every)
	j, _ = e.backup(every.ID)
	if j.Error != "" || j.SnapshotID == "" {
		t.Fatalf("backup of every machine: %s %q", j.Status, j.Error)
	}
	var sn tree.Snapshot
	e.do("GET", "/api/v1/snapshots/"+j.SnapshotID, nil, &sn)
	var names []string
	for _, s := range sn.Sources {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"DC0_H0_VM0", "DC0_H0_VM1"}) {
		t.Fatalf("machines in the snapshot: %v (warnings: %v)", names, j.Warnings)
	}
	left := 0
	for _, w := range j.Warnings {
		if strings.Contains(w, "was not backed up") {
			left++
		}
	}
	if want := len(sim.Machines()) - 2; left != want {
		t.Fatalf("%d machines were reported as left out, want %d: %v", left, want, j.Warnings)
	}
	for _, m := range sim.Machines() {
		if got := sim.Snapshots(m); len(got) != 0 {
			t.Fatalf("snapshots left on %s: %v", m, got)
		}
	}

	// Restore one machine of that snapshot under a new name. The server
	// is the one the policy names.
	var rj api.Job
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: sn.ID, Include: []string{"DC0_H0_VM1"}, VMName: "second restored"}, &rj)
	done := e.waitLong(rj.ID)
	if done.Status == api.StatusFailed {
		t.Fatalf("restore: %q %v", done.Error, done.Warnings)
	}
	if !slices.Contains(sim.Machines(), "second restored") {
		t.Fatalf("machines after the restore: %v", sim.Machines())
	}
	if !bytes.Equal(sim.Uploaded(t, "disk-0.vmdk"), second) || !bytes.Equal(sim.Uploaded(t, "disk-1.vmdk"), disk(3, 2)) {
		t.Fatal("the restored disks differ from the machine's")
	}
	// The same name again must fail before anything is uploaded.
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: sn.ID, Include: []string{"DC0_H0_VM1"}, VMName: "second restored"}, &rj)
	if done = e.waitLong(rj.ID); done.Status != api.StatusFailed || !strings.Contains(done.Error, "already has a virtual machine") {
		t.Fatalf("restore over an existing machine: %s %q", done.Status, done.Error)
	}

	// A server that policies use stays. Without its policy a restore
	// has to say where to.
	if err := e.admin.Do("DELETE", "/api/v1/vmware-servers/"+vs.ID, nil, nil); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("removing a server in use: %v", err)
	}
	e.do("DELETE", "/api/v1/policies/"+every.ID, nil, nil)
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: sn.ID, Include: []string{"DC0_H0_VM0"}, VMName: "nowhere", Target: "LocalDS_0"}, &rj)
	if done = e.waitLong(rj.ID); done.Status != api.StatusFailed || !strings.Contains(done.Error, "no VMware server") {
		t.Fatalf("restore without a server: %s %q", done.Status, done.Error)
	}
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: sn.ID, Include: []string{"DC0_H0_VM0"}, VMName: "first restored", Target: "LocalDS_0", VMwareServer: vs.ID}, &rj)
	if done = e.waitLong(rj.ID); done.Status == api.StatusFailed {
		t.Fatalf("restore to a named server: %q", done.Error)
	}
	if !bytes.Equal(sim.Uploaded(t, "disk-0.vmdk"), first) {
		t.Fatal("the restored disk differs from the machine's")
	}

	// The password stays on the server: no list shows it.
	var servers []api.VMwareServer
	e.do("GET", "/api/v1/vmware-servers", nil, &servers)
	noPassword("the list of servers", servers)
	var jobs []api.Job
	e.do("GET", "/api/v1/jobs?limit=100", nil, &jobs)
	noPassword("the list of jobs", jobs)
	var policies []api.Policy
	e.do("GET", "/api/v1/policies", nil, &policies)
	noPassword("the list of policies", policies)

	e.do("DELETE", "/api/v1/policies/"+p.ID, nil, nil)
	e.do("DELETE", "/api/v1/vmware-servers/"+vs.ID, nil, nil)
	e.do("GET", "/api/v1/vmware-servers", nil, &servers)
	if len(servers) != 0 {
		t.Fatalf("servers after removing the only one: %+v", servers)
	}
}
