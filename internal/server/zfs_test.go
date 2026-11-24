//go:build linux

package server_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ajthom90/better-backup/internal/adminclient"
	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/server"
	"github.com/ajthom90/better-backup/internal/tree"
	"github.com/ajthom90/better-backup/internal/truenas"
	"github.com/ajthom90/better-backup/internal/zfs"
)

// TestZFS backs up and restores real ZFS datasets the way an agent on
// TrueNAS SCALE does. It needs root and a pool to work in, named by
// BB_TEST_ZPOOL; everything it makes lives under one dataset there and
// is destroyed afterwards.
//
//	truncate -s 2G /tmp/pool.img && zpool create bbtest /tmp/pool.img
//	BB_TEST_ZPOOL=bbtest go test ./internal/server/ -run TestZFS -v
func TestZFS(t *testing.T) {
	pool := os.Getenv("BB_TEST_ZPOOL")
	if pool == "" {
		t.Skip("set BB_TEST_ZPOOL to a ZFS pool to run this test")
	}
	sh := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	base := pool + "/bbtest"
	exec.Command("zfs", "destroy", "-r", "-f", base).Run()
	root := filepath.Join(t.TempDir(), "mnt")
	sh("zfs", "create", "-o", "mountpoint="+root, base)
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-r", "-f", base).Run() })
	sh("zfs", "create", "-o", "snapdir=visible", base+"/shares")
	sh("zfs", "create", base+"/shares/media")
	sh("zfs", "create", "-o", "canmount=off", base+"/shares/unmounted")
	sh("zfs", "create", "-V", "24M", "-o", "volblocksize=16k", base+"/disk")
	dev := "/dev/zvol/" + base + "/disk"
	for i := 0; ; i++ {
		if _, err := os.Stat(dev); err == nil {
			break
		}
		if i > 100 {
			t.Fatal("the test volume's device did not appear")
		}
		time.Sleep(100 * time.Millisecond)
	}
	writeDev := func(data []byte, off int64) {
		t.Helper()
		f, err := os.OpenFile(dev, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	readDev := func() []byte {
		t.Helper()
		b, err := os.ReadFile(dev)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	writeDev(rnd(21, 3<<20), 5<<20)

	shares := filepath.Join(root, "shares")
	write(t, filepath.Join(shares, "report.txt"), []byte("as it was when the backup began"))
	write(t, filepath.Join(shares, "gone-later.txt"), []byte("deleted during the backup"))
	write(t, filepath.Join(shares, "media", "video.bin"), rnd(22, 2<<20))
	// A dataset's top folder has an owner and a mode of its own, which
	// differ from the empty folder it is mounted on.
	if err := os.Chmod(filepath.Join(shares, "media"), 0o750); err != nil {
		t.Fatal(err)
	}
	os.Chown(filepath.Join(shares, "media"), 4321, 4321)
	os.Chmod(shares, 0o770)

	// Permissions beyond owner and mode: a POSIX ACL that lets user 1234
	// read the report, a default ACL on a folder, and plain attributes
	// of the kind Samba keeps.
	sh("zfs", "set", "acltype=posix", "xattr=sa", base+"/shares")
	acl := func(entries ...[3]uint32) []byte {
		b := binary.LittleEndian.AppendUint32(nil, 2)
		for _, e := range entries {
			b = binary.LittleEndian.AppendUint16(b, uint16(e[0]))
			b = binary.LittleEndian.AppendUint16(b, uint16(e[1]))
			b = binary.LittleEndian.AppendUint32(b, e[2])
		}
		return b
	}
	const none = 0xFFFFFFFF
	fileACL := acl([3]uint32{1, 6, none}, [3]uint32{2, 4, 1234}, [3]uint32{4, 4, none}, [3]uint32{0x10, 4, none}, [3]uint32{0x20, 0, none})
	dirACL := acl([3]uint32{1, 7, none}, [3]uint32{2, 5, 1234}, [3]uint32{4, 5, none}, [3]uint32{0x10, 5, none}, [3]uint32{0x20, 0, none})
	wantAttrs := map[string]map[string][]byte{
		filepath.Join(shares, "report.txt"): {"system.posix_acl_access": fileACL, "user.DOSATTRIB": []byte("0x20\x00"), "user.comment": rnd(25, 3000)},
		filepath.Join(shares, "media"):      {"system.posix_acl_default": dirACL},
	}
	for path, attrs := range wantAttrs {
		for name, v := range attrs {
			if err := unix.Setxattr(path, name, v, 0); err != nil {
				t.Fatalf("setting %s on %s: %v", name, path, err)
			}
		}
	}

	// A snapshot of the user's own, visible under .zfs: it must not be
	// backed up as files, and must still be there afterwards.
	sh("zfs", "snapshot", base+"/shares@mine")
	// And one a backup that was cut short left behind.
	sh("zfs", "snapshot", "-o", zfs.Property+"=will-be-replaced", base+"/shares@better-backup-stale")
	wantDisk := readDev()

	// A TrueNAS system, as far as its configuration files go.
	nas := t.TempDir()
	write(t, filepath.Join(nas, "freenas-v1.db"), rnd(23, 300<<10))
	write(t, filepath.Join(nas, "pwenc_secret"), rnd(24, 32))
	write(t, filepath.Join(nas, "version"), []byte("25.04.2\n"))
	truenas.Database, truenas.SecretSeed = filepath.Join(nas, "freenas-v1.db"), filepath.Join(nas, "pwenc_secret")
	truenas.AuthorizedKeys, truenas.VersionFile = filepath.Join(nas, "none"), filepath.Join(nas, "version")

	// The zfs command, wrapped: right after the snapshots are taken, the
	// live data changes. A backup that reads the snapshots does not see it.
	wrap := filepath.Join(t.TempDir(), "zfs")
	real, err := exec.LookPath("zfs")
	if err != nil {
		t.Fatal(err)
	}
	write(t, wrap, []byte("#!/bin/sh\n"+real+` "$@" || exit $?
if [ "$1" = snapshot ] && [ -n "$BB_TEST_AFTER_SNAPSHOT" ]; then sh "$BB_TEST_AFTER_SNAPSHOT"; fi
exit 0
`))
	os.Chmod(wrap, 0o755)
	after := filepath.Join(t.TempDir(), "after.sh")
	write(t, after, []byte("echo 'changed after the snapshot' > "+shares+"/report.txt\nrm "+shares+"/gone-later.txt\n"+
		"echo new > "+shares+"/media/new.txt\ndd if=/dev/urandom of="+dev+" bs=1M count=2 seek=6 conv=notrunc,fsync 2>/dev/null\n"))
	zfs.Command = wrap
	t.Cleanup(func() { zfs.Command = "zfs" })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	data := t.TempDir()
	srv, err := server.New(server.Config{DataDir: data, Listen: "127.0.0.1:0", Log: log, Tick: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	sctx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan struct{})
	go func() { srv.Serve(sctx); srv.Close(); close(serverDone) }()
	defer func() { stopServer(); <-serverDone }()
	var af server.AdminFile
	readJSON(t, filepath.Join(data, "admin.json"), &af)
	admin, err := adminclient.New("https://"+srv.Addr(), af.Token, af.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, admin}
	var tok api.EnrollToken
	e.do("POST", "/api/v1/enroll-tokens", nil, &tok)
	adir := t.TempDir()
	cfg, err := agent.Enroll(adir, "https://"+srv.Addr(), tok.Token, tok.Fingerprint, "nas")
	if err != nil {
		t.Fatal(err)
	}
	sh("zfs", "set", zfs.Property+"="+cfg.AgentID, base+"/shares@better-backup-stale")
	ag, err := agent.New(adir, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { stopAgent(); <-agentDone }()

	var pol api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "nas", AgentID: cfg.AgentID, Paths: []string{shares},
		Volumes: []string{dev}, TrueNASConfig: true, Enabled: true}, &pol)
	t.Setenv("BB_TEST_AFTER_SNAPSHOT", after)
	var j api.Job
	e.do("POST", "/api/v1/policies/"+pol.ID+"/run", nil, &j)
	bj := e.wait(j.ID)
	if bj.Status != api.StatusSuccess {
		t.Fatalf("backup: %s %s %v", bj.Status, bj.Error, bj.Warnings)
	}
	t.Setenv("BB_TEST_AFTER_SNAPSHOT", "")
	if b, _ := os.ReadFile(filepath.Join(shares, "report.txt")); !strings.Contains(string(b), "after") {
		t.Fatal("the live files were not changed during the backup; the test proves nothing")
	}

	// Nothing of the backup's is left in the pool, and the user's own
	// snapshot is.
	left := sh("zfs", "list", "-H", "-r", "-t", "all", "-o", "name", base)
	if strings.Contains(left, "better-backup") || !strings.Contains(left, base+"/shares@mine") {
		t.Fatalf("datasets after the backup:\n%s", left)
	}

	// The snapshot holds the files as they were, and no .zfs folder.
	var ls []api.ListEntry
	e.do("GET", "/api/v1/snapshots/"+bj.SnapshotID+"/ls?path="+shares, nil, &ls)
	var names []string
	for _, l := range ls {
		names = append(names, l.Name)
	}
	if got := strings.Join(names, " "); got != "gone-later.txt media report.txt" {
		t.Fatalf("backed-up folder holds %q", got)
	}

	for _, l := range ls {
		if l.Name == "media" && l.Mode&0o777 != 0o750 {
			t.Fatalf("the child dataset's folder was recorded with mode %o", l.Mode&0o777)
		}
	}
	os.Chmod(filepath.Join(shares, "media"), 0o700)
	os.Chown(filepath.Join(shares, "media"), 0, 0)
	os.Chmod(shares, 0o700)
	for path, attrs := range wantAttrs {
		for name := range attrs {
			if err := unix.Removexattr(path, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Restore everything in place: the files and the volume go back to
	// the moment of the snapshot. The configuration says how it is
	// restored instead.
	var rj api.Job
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: bj.SnapshotID, Overwrite: "always"}, &rj)
	done := e.wait(rj.ID)
	if done.Status != api.StatusWarning || len(done.Warnings) != 1 || !strings.Contains(done.Warnings[0], "TrueNAS configuration") {
		t.Fatalf("restore in place: %s %s %v", done.Status, done.Error, done.Warnings)
	}
	if b, _ := os.ReadFile(filepath.Join(shares, "report.txt")); string(b) != "as it was when the backup began" {
		t.Fatalf("report.txt after the restore: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(shares, "gone-later.txt")); string(b) != "deleted during the backup" {
		t.Fatalf("gone-later.txt after the restore: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(shares, "media", "video.bin")); !bytes.Equal(b, rnd(22, 2<<20)) {
		t.Fatal("the file in the child dataset differs after the restore")
	}
	for path, attrs := range wantAttrs {
		for name, want := range attrs {
			got := make([]byte, 8192)
			n, err := unix.Getxattr(path, name, got)
			if err != nil || !bytes.Equal(got[:n], want) {
				t.Fatalf("%s of %s after the restore: %d bytes, %v", name, path, n, err)
			}
		}
	}
	if fi, err := os.Stat(filepath.Join(shares, "media")); err != nil || fi.Mode().Perm() != 0o750 || fi.Sys().(*syscall.Stat_t).Uid != 4321 {
		t.Fatalf("the child dataset's folder after the restore: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(shares); err != nil || fi.Mode().Perm() != 0o770 {
		t.Fatalf("the dataset's folder after the restore: %v %v", fi.Mode(), err)
	}
	if !bytes.Equal(readDev(), wantDisk) {
		t.Fatal("the volume differs from what it held when the backup began")
	}

	// The configuration restores to a folder as the file TrueNAS takes.
	out := t.TempDir()
	e.do("POST", "/api/v1/restores", api.RestoreRequest{SnapshotID: bj.SnapshotID, Include: []string{tree.TrueNASConfigName}, Target: out}, &rj)
	if done := e.wait(rj.ID); done.Status != api.StatusSuccess {
		t.Fatalf("restoring the configuration: %s %s %v", done.Status, done.Error, done.Warnings)
	}
	f, err := os.Open(filepath.Join(out, tree.TrueNASConfigName, truenas.ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	members := map[string][]byte{}
	for tr := tar.NewReader(f); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members[h.Name], _ = io.ReadAll(tr)
	}
	if len(members) != 2 || !bytes.Equal(members["freenas-v1.db"], rnd(23, 300<<10)) || !bytes.Equal(members["pwenc_secret"], rnd(24, 32)) {
		t.Fatalf("the configuration archive holds %d members", len(members))
	}
	if note, _ := os.ReadFile(filepath.Join(out, tree.TrueNASConfigName, "README.txt")); !strings.Contains(string(note), "25.04.2") {
		t.Fatalf("README.txt: %q", note)
	}
	if _, err := os.Stat(filepath.Join(adir, "truenas-config")); err == nil {
		t.Fatal("the agent kept its copy of the configuration, which holds the secret seed")
	}

	// Reading live files must still leave the visible .zfs folder out.
	var live api.Policy
	e.do("POST", "/api/v1/policies", api.Policy{Name: "live", AgentID: cfg.AgentID, Paths: []string{shares}, NoSnapshot: true, Enabled: true}, &live)
	e.do("POST", "/api/v1/policies/"+live.ID+"/run", nil, &j)
	if bj = e.wait(j.ID); bj.Status != api.StatusSuccess {
		t.Fatalf("live backup: %s %s %v", bj.Status, bj.Error, bj.Warnings)
	}
	e.do("GET", "/api/v1/snapshots/"+bj.SnapshotID+"/ls?path="+shares, nil, &ls)
	for _, l := range ls {
		if l.Name == ".zfs" {
			t.Fatal("the live backup walked into .zfs")
		}
	}
	if left := sh("zfs", "list", "-H", "-r", "-t", "snapshot", "-o", "name", base); left != base+"/shares@mine" {
		t.Fatalf("snapshots after the live backup:\n%s", left)
	}
}
