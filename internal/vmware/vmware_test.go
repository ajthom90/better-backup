package vmware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/vmware"
	"github.com/ajthom90/better-backup/internal/vmware/vmwaretest"
)

const block = vmwaretest.Block

// contents makes a disk of n blocks: random data with holes in it and
// a last block that is partly zeros.
func contents(n int) []byte {
	b := make([]byte, n*block)
	rnd := rand.New(rand.NewSource(int64(n)))
	for i := 0; i < n; i++ {
		if i%3 == 1 {
			continue
		}
		rnd.Read(b[i*block : (i+1)*block])
	}
	clear(b[len(b)-block/2:])
	return b
}

// read reads a frozen machine's disk the way a backup does: front to
// back, a block at a time.
func read(t *testing.T, fz *hypervisor.Frozen, i int) ([]byte, error) {
	t.Helper()
	d := fz.Disks[i]
	if d.Close != nil {
		defer d.Close()
	}
	out := make([]byte, d.Size)
	for off := int64(0); off < d.Size; off += block {
		if _, err := d.R.ReadAt(out[off:min(off+block, d.Size)], off); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return out, nil
}

func driver(t *testing.T) hypervisor.Driver {
	t.Helper()
	d := hypervisor.ByKind("vmware")
	if d == nil {
		t.Fatal("the VMware driver is not registered")
	}
	return d
}

func TestBackupAndRestore(t *testing.T) {
	sim := vmwaretest.Start(t)
	drv := driver(t)
	ctx := context.Background()
	want := contents(7)
	sim.Plant(t, "DC0_H0_VM0", want)

	names, err := drv.List(ctx, sim.Connection())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "DC0_H0_VM0") || !slices.IsSorted(names) {
		t.Fatalf("machines listed: %v", names)
	}

	fz, err := drv.Freeze(ctx, "DC0_H0_VM0", hypervisor.Options{Connection: sim.Connection(), Scope: "p1", Tag: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sim.Snapshots("DC0_H0_VM0"); !slices.Equal(got, []string{"better-backup p1 j1"}) {
		t.Fatalf("snapshots while frozen: %v", got)
	}
	if len(fz.Disks) != 1 || fz.Disks[0].Size != int64(len(want)) || fz.Disks[0].Name != "disk-0.vmdk" {
		t.Fatalf("disks: %+v", fz.Disks)
	}
	got, err := read(t, fz, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the disk that was read differs from the machine's")
	}
	if err := fz.Check(); err != nil {
		t.Fatal(err)
	}
	if err := fz.Release(); err != nil {
		t.Fatal(err)
	}
	if got := sim.Snapshots("DC0_H0_VM0"); len(got) != 0 {
		t.Fatalf("snapshots left after the backup: %v", got)
	}
	var cfg vmware.Config
	if err := json.Unmarshal(fz.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "DC0_H0_VM0" || cfg.Datastore != "LocalDS_0" || cfg.Host == "" || !strings.HasSuffix(cfg.Folder, "/vm") || cfg.Pool == "" {
		t.Fatalf("recorded place: %+v", cfg)
	}
	if strings.Contains(string(fz.Config), sim.Password) && sim.Password != "" && len(sim.Password) > 3 {
		t.Fatal("the configuration holds the password")
	}

	// A machine of that name exists: the restore must say so before
	// anything is made.
	if _, err := drv.NewRestore(ctx, fz.Config, hypervisor.Target{Connection: sim.Connection()}); err == nil || !strings.Contains(err.Error(), "already has a virtual machine") {
		t.Fatalf("restore over an existing machine: %v", err)
	}
	if _, err := drv.NewRestore(ctx, fz.Config, hypervisor.Target{Name: "back", Place: "nowhere", Connection: sim.Connection()}); err == nil || !strings.Contains(err.Error(), "LocalDS_0") {
		t.Fatalf("restore to a datastore that does not exist: %v", err)
	}

	rs, err := drv.NewRestore(ctx, fz.Config, hypervisor.Target{Name: "back", Connection: sim.Connection()})
	if err != nil {
		t.Fatal(err)
	}
	spec := hypervisor.DiskSpec{Name: "disk-0.vmdk", Size: int64(len(want)), BlockSize: block}
	for off := 0; off < len(want); off += block {
		spec.Data = append(spec.Data, len(bytes.Trim(want[off:off+block], "\x00")) > 0)
	}
	w, fresh, done, err := rs.Disk(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("a new disk must count as reading zeros")
	}
	for i, has := range spec.Data {
		if has {
			if _, err := w.WriteAt(want[i*block:(i+1)*block], int64(i)*block); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	warnings, err := rs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "MAC") }) {
		t.Fatalf("warnings: %v", warnings)
	}
	if !slices.Contains(sim.Machines(), "back") {
		t.Fatalf("machines after the restore: %v", sim.Machines())
	}
	if !bytes.Equal(sim.Uploaded(t, "disk-0.vmdk"), want) {
		t.Fatal("the uploaded disk differs from the backup")
	}
	if !strings.Contains(rs.Where(), "LocalDS_0") {
		t.Fatalf("where: %s", rs.Where())
	}
}

// TestSeveralDisks backs up a machine with disks of different sizes:
// each must come out as itself, though they are downloaded one after
// the other.
func TestSeveralDisks(t *testing.T) {
	sim := vmwaretest.Start(t)
	drv := driver(t)
	ctx := context.Background()
	want := [][]byte{contents(3), contents(6), contents(2)}
	sim.Plant(t, "DC0_C0_RP0_VM0", want...)
	fz, err := drv.Freeze(ctx, "DC0_C0_RP0_VM0", hypervisor.Options{Connection: sim.Connection(), Scope: "p", Tag: "j"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fz.Disks) != len(want) {
		t.Fatalf("%d disks, want %d", len(fz.Disks), len(want))
	}
	for i := range want {
		got, err := read(t, fz, i)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("disk %d differs", i)
		}
	}
	if err := fz.Check(); err != nil {
		t.Fatal(err)
	}
	if err := fz.Release(); err != nil {
		t.Fatal(err)
	}

	rs, err := drv.NewRestore(ctx, fz.Config, hypervisor.Target{Name: "three disks", Connection: sim.Connection()})
	if err != nil {
		t.Fatal(err)
	}
	for i, disk := range want {
		spec := hypervisor.DiskSpec{Name: fz.Disks[i].Name, Size: int64(len(disk)), BlockSize: block}
		for off := 0; off < len(disk); off += block {
			spec.Data = append(spec.Data, len(bytes.Trim(disk[off:off+block], "\x00")) > 0)
		}
		w, _, done, err := rs.Disk(spec)
		if err != nil {
			t.Fatal(err)
		}
		for b, has := range spec.Data {
			if has {
				if _, err := w.WriteAt(disk[b*block:(b+1)*block], int64(b)*block); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := done(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rs.Finish(); err != nil {
		t.Fatal(err)
	}
	for i, disk := range want {
		if !bytes.Equal(sim.Uploaded(t, fz.Disks[i].Name), disk) {
			t.Fatalf("uploaded disk %d differs from the backup", i)
		}
	}

	// A restore that stops half way takes the machine away again.
	rs, err = drv.NewRestore(ctx, fz.Config, hypervisor.Target{Name: "given up", Connection: sim.Connection()})
	if err != nil {
		t.Fatal(err)
	}
	spec := hypervisor.DiskSpec{Name: fz.Disks[0].Name, Size: int64(len(want[0])), BlockSize: block, Data: []bool{true, true, true}}
	w, _, done, err := rs.Disk(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(want[0][:block], 0); err != nil {
		t.Fatal(err)
	}
	if err := done(); err == nil {
		t.Fatal("a disk that was not written to its end was accepted")
	}
	rs.(hypervisor.Aborter).Abort()
}

func TestBrokenDownload(t *testing.T) {
	sim := vmwaretest.Start(t)
	drv := driver(t)
	ctx := context.Background()
	sim.Plant(t, "DC0_H0_VM1", contents(5))
	// A snapshot an interrupted backup left behind goes first.
	fz, err := drv.Freeze(ctx, "DC0_H0_VM1", hypervisor.Options{Connection: sim.Connection(), Scope: "p1", Tag: "old"})
	if err != nil {
		t.Fatal(err)
	}
	_ = fz // never released, as after a crash
	other, err := drv.Freeze(ctx, "DC0_H0_VM1", hypervisor.Options{Connection: sim.Connection(), Scope: "p2", Tag: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	_ = other

	sim.Truncate(t, "DC0_H0_VM1", 3*block)
	fz, err = drv.Freeze(ctx, "DC0_H0_VM1", hypervisor.Options{Connection: sim.Connection(), Scope: "p1", Tag: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sim.Snapshots("DC0_H0_VM1"); slices.Contains(got, "better-backup p1 old") || !slices.Contains(got, "better-backup p2 theirs") || !slices.Contains(got, "better-backup p1 new") {
		t.Fatalf("snapshots: %v", got)
	}
	_, rerr := read(t, fz, 0)
	cerr := fz.Check()
	if rerr == nil && cerr == nil {
		t.Fatal("a download that broke off went unnoticed")
	}
	if cerr == nil {
		t.Fatal("the check passed although the disk could not be read")
	}
	if err := fz.Release(); err != nil {
		t.Fatal(err)
	}
	if got := sim.Snapshots("DC0_H0_VM1"); slices.Contains(got, "better-backup p1 new") {
		t.Fatalf("snapshots after release: %v", got)
	}
}

func TestConnect(t *testing.T) {
	sim := vmwaretest.Start(t)
	drv := driver(t)
	ctx := context.Background()

	if _, err := drv.List(ctx, nil); err == nil || !strings.Contains(err.Error(), "no VMware server") {
		t.Fatalf("no connection: %v", err)
	}
	c := sim.Connection()
	c.Thumbprint = ""
	_, err := drv.List(ctx, c)
	if err == nil || !strings.Contains(err.Error(), "does not trust") || !strings.Contains(strings.ReplaceAll(err.Error(), ":", ""), sim.Thumbprint) {
		t.Fatalf("unknown certificate: %v", err)
	}
	c.Thumbprint = strings.Repeat("AB", 32)
	if _, err := drv.List(ctx, c); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("wrong thumbprint: %v", err)
	}
	c.Thumbprint, c.Insecure = "", true
	if _, err := drv.List(ctx, c); err != nil {
		t.Fatalf("insecure: %v", err)
	}
	c = sim.Connection()
	c.Password = "" // the simulator takes any password but none
	if _, err := drv.List(ctx, c); err == nil || !strings.Contains(err.Error(), "does not accept the user name and password") {
		t.Fatalf("wrong password: %v", err)
	}
	// Thumbprints are accepted the way people paste them.
	c = sim.Connection()
	var pairs []string
	for i := 0; i < len(sim.Thumbprint); i += 2 {
		pairs = append(pairs, strings.ToLower(sim.Thumbprint[i:i+2]))
	}
	c.Thumbprint = strings.Join(pairs, ":")
	if _, err := drv.List(ctx, c); err != nil {
		t.Fatalf("thumbprint with colons: %v", err)
	}
	if _, err := drv.Freeze(ctx, "no such machine", hypervisor.Options{Connection: sim.Connection()}); err == nil || !strings.Contains(err.Error(), "no virtual machine named") {
		t.Fatalf("missing machine: %v", err)
	}
}
