package recovery_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
)

// TestRecoveryDisc is the whole of bare-metal recovery with nothing
// faked. It builds two small machines that start by themselves (one
// through UEFI from a GPT disk, one through BIOS from an MBR disk with a
// logical partition), backs each up whole, then for each starts a blank
// virtual machine from the real recovery disc, approves it by the code
// on its screen, restores onto its larger blank disk, and finally starts
// the restored disk to see the machine come up.
//
// It needs the recovery disc (set BB_RECOVERY_ISO to the image made by
// packaging/recovery/build.py), QEMU, OVMF, GRUB and a few disk tools,
// so it only runs when asked for.
func TestRecoveryDisc(t *testing.T) {
	iso := os.Getenv("BB_RECOVERY_ISO")
	if iso == "" {
		t.Skip("set BB_RECOVERY_ISO to a recovery disc image to run this test")
	}
	for _, tool := range []string{"qemu-system-x86_64", "sgdisk", "sfdisk", "mke2fs", "mkswap", "mformat", "mcopy", "mmd", "grub-mkstandalone", "grub-mkimage"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is needed for this test", tool)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 200 * time.Millisecond, RecoveryISO: iso})
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
	_, port, _ := strings.Cut(srv.Addr(), ":")
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New(base, af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, in, out any) {
		t.Helper()
		if err := admin.Do(method, path, in, out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	wait := func(id string, limit time.Duration) *api.Job {
		t.Helper()
		for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			var j api.Job
			do("GET", "/api/v1/jobs/"+id, nil, &j)
			if j.Status != api.StatusQueued && j.Status != api.StatusRunning {
				return &j
			}
		}
		t.Fatalf("job %s did not finish", id)
		return nil
	}

	// The agent of the machines to protect.
	var tok api.EnrollToken
	do("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	if _, err := agent.Enroll(adir, base, tok.Token, tok.Fingerprint, "source"); err != nil {
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
	do("GET", "/api/v1/agents", nil, &agents)

	// The recovery disc as an administrator gets it: from the server,
	// with the address the machine is to use. From inside QEMU's
	// network the host is 10.0.2.2.
	var link struct{ URL string }
	do("POST", "/api/v1/recovery/iso/link", map[string]string{"server": "https://10.0.2.2:" + port}, &link)
	disc := filepath.Join(t.TempDir(), "recovery.iso")
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := insecure.Get(base + link.URL)
	if err != nil {
		t.Fatal(err)
	}
	df, _ := os.Create(disc)
	if _, err := io.Copy(df, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	df.Close()

	for _, fw := range []string{"uefi", "bios"} {
		t.Run(fw, func(t *testing.T) {
			dir := t.TempDir()
			marker := "the " + fw + " machine is alive " + time.Now().Format(time.RFC3339Nano)
			src := filepath.Join(dir, "source.img")
			makeMachine(t, fw, src, marker)
			if out := boot(t, fw, dir, src, "", 5*time.Minute); !strings.Contains(out, marker) {
				t.Fatalf("the machine built for the test does not start by itself:\n%s", out)
			}

			var pol api.Policy
			do("POST", "/api/v1/policies", api.Policy{Name: fw, AgentID: agents[0].ID, BareMetal: true, MachineDisks: []string{src}}, &pol)
			var job api.Job
			do("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &job)
			if j := wait(job.ID, time.Minute); j.Status != api.StatusSuccess {
				t.Fatalf("backup: %s %s %v", j.Status, j.Error, j.Warnings)
			}
			var snaps []tree.Snapshot
			do("GET", "/api/v1/snapshots?policy_id="+pol.ID, nil, &snaps)
			if len(snaps) != 1 {
				t.Fatalf("%d snapshots", len(snaps))
			}

			// A blank machine with a larger disk, started from the disc.
			target := filepath.Join(dir, "target.img")
			tf, _ := os.Create(target)
			tf.Truncate(96 << 20)
			tf.Close()
			serial := filepath.Join(dir, "recovery-serial.log")
			vm := qemu(t, fw, dir, target, disc, serial)
			if err := vm.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- vm.Wait() }()
			defer vm.Process.Kill()
			screen := func() string { b, _ := os.ReadFile(serial); return clean(string(b)) }
			var m *api.RecoveryMachine
			until := func(what string, limit time.Duration, ok func(m *api.RecoveryMachine) bool) {
				t.Helper()
				for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
					var rs api.RecoveryStatus
					do("GET", "/api/v1/recovery", nil, &rs)
					if len(rs.Machines) == 1 && ok(&rs.Machines[0]) {
						m = &rs.Machines[0]
						return
					}
					select {
					case err := <-exited:
						t.Fatalf("the virtual machine stopped while waiting for %s: %v\n%s", what, err, screen())
					default:
					}
				}
				t.Fatalf("timed out waiting for %s. The machine's screen:\n%s", what, screen())
			}
			until("the machine to announce itself", 10*time.Minute, func(m *api.RecoveryMachine) bool { return m.Hardware != nil })
			code := regexp.MustCompile(`Recovery code:\s+([A-Z0-9]{3}-[A-Z0-9]{3})`).FindStringSubmatch(screen())
			if code == nil || code[1] != m.Code {
				t.Fatalf("the server knows the machine as %s; its screen says:\n%s", m.Code, screen())
			}
			if m.Hardware.Firmware != fw || len(m.Hardware.Disks) != 1 || m.Hardware.Disks[0].Size != 96<<20 || m.Hardware.Disks[0].Media {
				t.Fatalf("hardware: %+v", m.Hardware)
			}
			do("POST", "/api/v1/recovery/machines/"+m.ID+"/approve", nil, nil)
			until("the machine to sign on", 2*time.Minute, func(m *api.RecoveryMachine) bool { return m.AgentID != "" })
			do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: snaps[0].ID, AgentID: m.AgentID, Disks: map[string]string{"disk0": m.Hardware.Disks[0].Path}}, &job)
			if j := wait(job.ID, 10*time.Minute); j.Status != api.StatusSuccess {
				t.Fatalf("restore: %s %s %v\n%s", j.Status, j.Error, j.Warnings, screen())
			}
			until("the machine to report its restored disk", time.Minute, func(m *api.RecoveryMachine) bool {
				return strings.Contains(m.Status, "finished") && m.Hardware != nil && strings.Contains(m.Hardware.Disks[0].Contents, "partitions")
			})
			do("POST", "/api/v1/recovery/machines/"+m.ID+"/command", map[string]string{"command": "poweroff"}, nil)
			select {
			case <-exited:
			case <-time.After(2 * time.Minute):
				t.Fatalf("the machine did not switch off:\n%s", screen())
			}
			do("DELETE", "/api/v1/recovery/machines/"+m.ID, nil, nil)

			// The restored disk has the original's table, moved to the
			// end of the larger disk where it must be, and starts.
			a, b := table(t, src), table(t, target)
			if len(a.Partitions) != len(b.Partitions) || a.GUID != b.GUID {
				t.Fatalf("partition tables differ:\n%+v\n%+v", a, b)
			}
			for i := range a.Partitions {
				x, y := a.Partitions[i], b.Partitions[i]
				if x.Offset != y.Offset || x.Size != y.Size || x.Type != y.Type || x.GUID != y.GUID || x.Active != y.Active {
					t.Fatalf("partition %d differs: %+v and %+v", x.Number, x, y)
				}
			}
			if fw == "uefi" {
				if out, err := exec.Command("sgdisk", "-v", target).CombinedOutput(); err != nil || !bytes.Contains(out, []byte("No problems found")) {
					t.Fatalf("sgdisk finds fault with the restored disk: %v\n%s", err, out)
				}
			}
			if out := boot(t, fw, dir, target, "", 5*time.Minute); !strings.Contains(out, marker) {
				t.Fatalf("the restored machine does not start:\n%s", out)
			}
		})
	}
}

func table(t *testing.T, path string) *disklayout.Disk {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	d, err := disklayout.Read(f, fi.Size(), 0)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return d
}

var escapes = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b[()][A-Z0-9]|\r`)

func clean(s string) string { return escapes.ReplaceAllString(s, "") }

func sh(t *testing.T, dir, stdin string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func place(t *testing.T, disk, part string, offset int64) {
	t.Helper()
	b, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, offset); err != nil {
		t.Fatal(err)
	}
}

// makeMachine builds a disk that starts on its own: its boot loader
// finds a file on another partition, prints it on the serial port and
// switches the machine off. That takes a working boot path, partition
// table and file systems, which is what a restore has to put back.
func makeMachine(t *testing.T, fw, disk, marker string) {
	t.Helper()
	dir := filepath.Dir(disk)
	const mib = 1 << 20
	f, _ := os.Create(disk)
	f.Truncate(64 * mib)
	f.Close()
	os.MkdirAll(filepath.Join(dir, "fs"), 0o755)
	os.WriteFile(filepath.Join(dir, "fs", "marker.txt"), []byte(marker+"\n"), 0o644)
	cfg := "serial --unit=0 --speed=115200\nterminal_output serial\nsearch --no-floppy --file --set=d /marker.txt\necho BB-E2E-BEGIN\ncat ($d)/marker.txt\necho BB-E2E-END\nhalt\n"
	os.WriteFile(filepath.Join(dir, "grub.cfg"), []byte(cfg), 0o644)
	ext2 := func(name string, size int64, from string) string {
		p := filepath.Join(dir, name)
		args := []string{"-q", "-F", "-t", "ext2"}
		if from != "" {
			args = append(args, "-d", from)
		}
		sh(t, dir, "", "mke2fs", append(args, p, fmt.Sprintf("%dk", size/1024))...)
		return p
	}
	if fw == "uefi" {
		sh(t, dir, "", "sgdisk", "-n", "1:2048:+20M", "-t", "1:ef00", "-c", "1:EFI system partition", "-n", "2:0:+4M", "-t", "2:8200", "-n", "3:0:0", "-t", "3:8300", disk)
		d := table(t, disk)
		sh(t, dir, "", "grub-mkstandalone", "-O", "x86_64-efi", "-o", "bootx64.efi", "--locales=", "--fonts=", "--themes=",
			"--modules=part_gpt ext2 serial search search_fs_file cat echo halt", "--install-modules=part_gpt ext2 fat serial terminal search search_fs_file cat echo halt normal configfile", "boot/grub/grub.cfg=grub.cfg")
		esp := filepath.Join(dir, "esp.img")
		ef, _ := os.Create(esp)
		ef.Truncate(d.Partitions[0].Size)
		ef.Close()
		sh(t, dir, "", "mformat", "-i", esp, "::")
		sh(t, dir, "", "mmd", "-i", esp, "::/EFI", "::/EFI/BOOT")
		sh(t, dir, "", "mcopy", "-i", esp, "bootx64.efi", "::/EFI/BOOT/BOOTX64.EFI")
		place(t, disk, esp, d.Partitions[0].Offset)
		swap := filepath.Join(dir, "swap.img")
		sf, _ := os.Create(swap)
		sf.Truncate(d.Partitions[1].Size)
		sf.Close()
		sh(t, dir, "", "mkswap", swap)
		place(t, disk, swap, d.Partitions[1].Offset)
		place(t, disk, ext2("data.img", d.Partitions[2].Size, "fs"), d.Partitions[2].Offset)
		return
	}
	// BIOS: boot code in the first sector, GRUB's core in the gap
	// before the first partition, and the file on a logical partition.
	sh(t, dir, "label: dos\nstart=2048, size=24M, type=83, bootable\ntype=5\nsize=8M, type=83\n", "sfdisk", "-q", disk)
	d := table(t, disk)
	if len(d.Partitions) != 3 || !d.Partitions[2].Logical {
		t.Fatalf("unexpected MBR layout: %+v", d.Partitions)
	}
	pc := "/usr/lib/grub/i386-pc"
	sh(t, dir, "", "grub-mkimage", "-O", "i386-pc", "-d", pc, "-o", "core.img", "-c", "grub.cfg", "-p", "/boot/grub",
		"biosdisk", "part_msdos", "ext2", "serial", "terminal", "search", "search_fs_file", "cat", "echo", "halt")
	bootImg, err := os.ReadFile(pc + "/boot.img")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "boot440.img"), bootImg[:440], 0o644)
	place(t, disk, filepath.Join(dir, "boot440.img"), 0)
	place(t, disk, filepath.Join(dir, "core.img"), 512)
	place(t, disk, ext2("first.img", d.Partitions[0].Size, ""), d.Partitions[0].Offset)
	place(t, disk, ext2("data.img", d.Partitions[2].Size, "fs"), d.Partitions[2].Offset)
}

// qemu describes a virtual machine with one disk and, if cdrom is set,
// the recovery disc and a network adapter.
func qemu(t *testing.T, fw, dir, disk, cdrom, serial string) *exec.Cmd {
	t.Helper()
	args := []string{"-machine", "q35", "-m", "1536", "-smp", "2", "-display", "none", "-no-reboot", "-serial", "file:" + serial,
		"-drive", "file=" + disk + ",format=raw,if=none,id=d0", "-device", "virtio-scsi-pci", "-device", "scsi-hd,drive=d0,bootindex=1"}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		args = append(args, "-accel", "kvm", "-cpu", "host")
	} else {
		args = append(args, "-accel", "tcg")
	}
	if fw == "uefi" {
		vars := filepath.Join(dir, "vars-"+filepath.Base(disk)+".fd")
		b, err := os.ReadFile("/usr/share/OVMF/OVMF_VARS_4M.fd")
		if err != nil {
			t.Fatalf("OVMF is needed for this test: %v", err)
		}
		os.WriteFile(vars, b, 0o644)
		args = append(args, "-drive", "if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd", "-drive", "if=pflash,format=raw,file="+vars)
	}
	if cdrom != "" {
		args = append(args, "-drive", "file="+cdrom+",format=raw,if=none,id=cd,media=cdrom,readonly=on", "-device", "scsi-cd,drive=cd,bootindex=2",
			"-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0")
	}
	return exec.Command("qemu-system-x86_64", args...)
}

// boot starts a machine from its disk and returns what it printed.
func boot(t *testing.T, fw, dir, disk, cdrom string, limit time.Duration) string {
	t.Helper()
	serial := filepath.Join(dir, "boot-"+filepath.Base(disk)+".log")
	os.Remove(serial)
	vm := qemu(t, fw, dir, disk, cdrom, serial)
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { vm.Wait(); close(done) }()
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(time.Second) {
		b, _ := os.ReadFile(serial)
		if strings.Contains(string(b), "BB-E2E-END") {
			break
		}
		select {
		case <-done:
			end = time.Now()
		default:
		}
	}
	vm.Process.Kill()
	<-done
	b, _ := os.ReadFile(serial)
	return clean(string(b))
}
