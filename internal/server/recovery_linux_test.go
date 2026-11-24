package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/recovery"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestBareMetalRecovery backs up a whole machine (a disk image file
// stands in for its disk), then plays a machine started from the
// recovery disc: it announces itself, is approved by its code, receives
// the machine onto a blank disk and is told to restart.
func TestBareMetalRecovery(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { err := srv.Serve(ctx); srv.Close(); served <- err }()
	defer func() { stop(); <-served }()
	base := "https://" + srv.Addr()
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}

	// The machine to protect: a disk with a GPT, a boot partition and a
	// data partition.
	const mib = 1 << 20
	disk := make([]byte, 24*mib)
	copy(disk[1*mib:], rnd(1, 2*mib))
	copy(disk[5*mib:], rnd(2, 16*mib))
	src := filepath.Join(t.TempDir(), "disk.img")
	write(t, src, disk)
	layout := &disklayout.Disk{Size: int64(len(disk)), SectorSize: 512, Style: disklayout.StyleGPT, GUID: "01234567-89AB-CDEF-0123-456789ABCDEF", Entries: 128, FirstUsable: 34,
		Head: make([]byte, 512), Partitions: []disklayout.Partition{
			{Number: 1, Offset: 1 * mib, Size: 4 * mib, Type: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", GUID: "00000000-0000-0000-0000-000000000001", Name: "EFI system partition"},
			{Number: 2, Offset: 5 * mib, Size: 16 * mib, Type: "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7", GUID: "00000000-0000-0000-0000-000000000002", Name: "Basic data partition"}}}
	f, err := os.OpenFile(src, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Write(f, int64(len(disk))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	disk, _ = os.ReadFile(src)

	var tok api.EnrollToken
	e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, base, tok.Token, tok.Fingerprint, "fileserver"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(adir, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { stopAgent(); <-agentDone }()
	var agents []api.Agent
	e.do("GET", "/api/v1/agents", nil, &agents)

	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "whole machine", AgentID: agents[0].ID, BareMetal: true, MachineDisks: []string{src}}, &pol)
	var job api.Job
	e.do("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &job)
	if j := e.wait(job.ID); j.Status != api.StatusSuccess {
		t.Fatalf("backup: %s %s %v", j.Status, j.Error, j.Warnings)
	}
	var snaps []tree.Snapshot
	e.do("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 1 || snaps[0].Sources[0].Type != tree.TypeMachine {
		t.Fatalf("snapshots: %+v", snaps)
	}
	var mb api.MachineBackup
	e.do("GET", "/api/v1/snapshots/"+snaps[0].ID+"/machine", nil, &mb)
	if len(mb.Disks) != 1 || mb.Disks[0].Name != "disk0" || len(mb.Disks[0].Partitions) != 2 || !mb.Disks[0].Partitions[1].Saved || mb.Disks[0].Partitions[0].Kind != disklayout.KindESP {
		t.Fatalf("machine backup: %+v", mb)
	}
	var listing []api.ListEntry
	e.do("GET", "/api/v1/snapshots/"+snaps[0].ID+"/ls?path="+strings.ReplaceAll(tree.MachineName, " ", "%20"), nil, &listing)
	if len(listing) != 3 {
		t.Fatalf("listing of the machine: %+v", listing)
	}

	// The machine at the recovery disc.
	dest := filepath.Join(t.TempDir(), "blank-disk.img")
	var mu sync.Mutex
	var state recovery.State
	commands := make(chan string, 4)
	rctx, stopRecovery := context.WithCancel(context.Background())
	recoveryDone := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		recovery.Run(rctx, recovery.Options{Config: api.RecoveryConfig{Server: base, Fingerprint: af.Fingerprint}, Version: "test", Dir: filepath.Join(t.TempDir(), "id"), Log: log,
			Hardware: func() *api.RecoveryHardware {
				return &api.RecoveryHardware{Product: "Test machine", Firmware: "uefi", Disks: []api.RecoveryDisk{{Path: dest, Size: 32 * mib, SectorSize: 512, Contents: "empty"}}}
			},
			Show:    func(s recovery.State) { mu.Lock(); state = s; mu.Unlock() },
			Command: func(c string) { commands <- c }})
	}()
	defer func() { stopRecovery(); <-recoveryDone }()
	until := func(what string, ok func(m *api.RecoveryMachine) bool) *api.RecoveryMachine {
		t.Helper()
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			var rs api.RecoveryStatus
			e.do("GET", "/api/v1/recovery", nil, &rs)
			if len(rs.Machines) == 1 && ok(&rs.Machines[0]) {
				return &rs.Machines[0]
			}
		}
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
	m := until("the machine to announce itself", func(m *api.RecoveryMachine) bool { return m.Hardware != nil })
	mu.Lock()
	code := state.Code
	mu.Unlock()
	if m.Code == "" || m.Code != code || m.Approved || m.AgentID != "" || m.Hardware.Disks[0].Path != dest {
		t.Fatalf("waiting machine: %+v, code on its screen %q", m, code)
	}
	// Nothing can be sent to it before it is approved.
	if err := admin.Do("POST", "/api/v1/recovery/machines/"+m.ID+"/command", map[string]string{"command": "reboot"}, nil); err == nil {
		t.Fatal("a command was accepted for a machine that is not approved")
	}
	e.do("POST", "/api/v1/recovery/machines/"+m.ID+"/approve", nil, nil)
	m = until("the machine to sign on", func(m *api.RecoveryMachine) bool { return m.AgentID != "" })

	// It is not a machine to back up, and does not count as one.
	e.do("GET", "/api/v1/agents", nil, &agents)
	n := 0
	for _, a := range agents {
		if a.Recovery {
			n++
			if a.ID != m.AgentID {
				t.Fatalf("unexpected recovery agent %+v", a)
			}
		}
	}
	var st api.Status
	e.do("GET", "/api/v1/status", nil, &st)
	if n != 1 || st.Agents != 1 {
		t.Fatalf("%d recovery agents listed, %d agents counted", n, st.Agents)
	}
	if err := admin.Do("POST", "/api/v1/policies", api.Policy{Name: "x", AgentID: m.AgentID, Paths: []string{"/"}}, nil); err == nil {
		t.Fatal("a policy was accepted for a recovery machine")
	}
	// Only a whole machine can go to it, and only onto named disks.
	if err := admin.Do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: snaps[0].ID, AgentID: m.AgentID, Target: "/tmp/x"}, nil); err == nil {
		t.Fatal("a file restore to a recovery machine was accepted")
	}
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: snaps[0].ID, AgentID: m.AgentID, Disks: map[string]string{"disk0": dest}}, &job)
	if j := e.wait(job.ID); j.Status != api.StatusWarning && j.Status != api.StatusSuccess {
		t.Fatalf("restore: %s %s %v", j.Status, j.Error, j.Warnings)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, disk) {
		t.Fatal("the restored disk differs from the original")
	}
	m = until("the restore to show on the machine", func(m *api.RecoveryMachine) bool {
		return m.Job != nil && m.Job.ID == job.ID && strings.Contains(m.Status, "finished")
	})
	e.do("POST", "/api/v1/recovery/machines/"+m.ID+"/command", map[string]string{"command": "reboot"}, nil)
	select {
	case c := <-commands:
		if c != "reboot" {
			t.Fatalf("the machine was told to %q", c)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the machine was never told to restart")
	}
	// Forgetting the machine blocks the identity it was given.
	e.do("DELETE", "/api/v1/recovery/machines/"+m.ID, nil, nil)
	e.do("GET", "/api/v1/agents", nil, &agents)
	for _, a := range agents {
		if a.Recovery && !a.Revoked {
			t.Fatalf("the recovery machine's identity still works: %+v", a)
		}
	}

	// The recovery disc image: uploaded, then handed out with this
	// server's address written into it.
	iso := append(rnd(7, 3*mib), []byte("linux /vmlinuz quiet "+server.RecoveryPlaceholder+strings.Repeat("@", server.RecoveryConfigSize-len(server.RecoveryPlaceholder))+" end\n")...)
	iso = append(iso, rnd(8, mib)...)
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	put := func(body []byte) int {
		req, _ := http.NewRequest("PUT", base+"/api/v1/recovery/iso", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+af.Token)
		resp, err := insecure.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := put(rnd(9, 2*mib)); c != http.StatusBadRequest {
		t.Fatalf("uploading something that is no recovery disc: status %d", c)
	}
	if c := put(iso); c != http.StatusNoContent {
		t.Fatalf("uploading the recovery disc: status %d", c)
	}
	var link struct{ URL string }
	if err := admin.Do("POST", "/api/v1/recovery/iso/link", map[string]string{"server": "http://x"}, &link); err == nil {
		t.Fatal("a server address without https was accepted")
	}
	e.do("POST", "/api/v1/recovery/iso/link", map[string]string{"server": "https://backup.example.com:8443"}, &link)
	resp, err := insecure.Get(base + link.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := "linux /vmlinuz quiet bb.server=https://backup.example.com:8443 bb.fingerprint=" + af.Fingerprint
	if resp.StatusCode != 200 || len(body) != len(iso) || !bytes.Contains(body, []byte(want)) || bytes.Contains(body, []byte(server.RecoveryPlaceholder)) ||
		!bytes.Equal(body[:3*mib], iso[:3*mib]) || !bytes.HasSuffix(body, append([]byte(" end\n"), iso[len(iso)-mib:]...)) {
		t.Fatalf("downloaded image: status %d, %d bytes (want %d)", resp.StatusCode, len(body), len(iso))
	}
}

// TestPersonalizeISO writes the server's address over placeholders
// wherever they fall between the pieces the image is read in.
func TestPersonalizeISO(t *testing.T) {
	ph := server.RecoveryPlaceholder + strings.Repeat("@", server.RecoveryConfigSize-len(server.RecoveryPlaceholder))
	cfg := api.RecoveryConfig{Server: "https://10.0.0.5:8443", Fingerprint: "sha256:abcd"}
	fill := "bb.server=https://10.0.0.5:8443 bb.fingerprint=sha256:abcd"
	fill += strings.Repeat(" ", server.RecoveryConfigSize-len(fill))
	for _, at := range []int{0, 1, 1<<20 - 400, 1<<20 - 10, 1<<20 - 1, 1 << 20, 1<<20 + 1, 2<<20 - 384, 3 << 20} {
		in := append(append(rnd(3, at), ph...), rnd(4, 5000)...)
		in = append(append(in, ph...), "tail"...)
		var out bytes.Buffer
		if err := server.PersonalizeISO(&out, &slowReader{bytes.NewReader(in)}, cfg); err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(string(in), ph, fill)
		if out.String() != want {
			t.Fatalf("placeholder at %d: output differs (%d bytes, want %d)", at, out.Len(), len(want))
		}
	}
}

// slowReader hands out odd-sized pieces.
type slowReader struct{ r io.Reader }

func (s *slowReader) Read(p []byte) (int, error) {
	return s.r.Read(p[:min(len(p), 777_777)])
}
