package restore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// fakeHV is a hypervisor that records what a restore asks of it.
type fakeHV struct {
	exists     bool  // a machine of the name is there
	finishErr  error // what Finish fails with
	asideErr   error // what SetAside fails with
	calls      []string
	target     hypervisor.Target
	wrote      int
	hasReplace bool
}

type fakeDriver struct{ h *fakeHV }

func (d fakeDriver) Kind() string       { return "faketest" }
func (d fakeDriver) Label() string      { return "Fake" }
func (d fakeDriver) ConfigFile() string { return "fake.vmcfg" }
func (d fakeDriver) Available() error   { return nil }
func (d fakeDriver) List(context.Context, *hypervisor.Connection) ([]string, error) {
	return nil, nil
}
func (d fakeDriver) Freeze(context.Context, string, hypervisor.Options) (*hypervisor.Frozen, error) {
	return nil, errors.New("not in this test")
}
func (d fakeDriver) NewRestore(_ context.Context, _ []byte, t hypervisor.Target) (hypervisor.Restore, error) {
	d.h.calls = append(d.h.calls, "new")
	d.h.target = t
	return fakeRestore{d.h}, nil
}

type fakeRestore struct{ h *fakeHV }

func (r fakeRestore) Where() string { return "there" }
func (r fakeRestore) Disk(hypervisor.DiskSpec) (io.WriterAt, bool, func() error, error) {
	r.h.calls = append(r.h.calls, "disk")
	return r.h, true, func() error { return nil }, nil
}
func (r fakeRestore) Finish() ([]string, error) {
	r.h.calls = append(r.h.calls, "finish")
	return nil, r.h.finishErr
}
func (h *fakeHV) WriteAt(p []byte, _ int64) (int, error) { h.wrote += len(p); return len(p), nil }

// fakeReplacer is fakeDriver with the means to replace a machine.
type fakeReplacer struct{ fakeDriver }

func (d fakeReplacer) Find(_ context.Context, _ *hypervisor.Connection, name string) (string, bool, error) {
	d.h.calls = append(d.h.calls, "find "+name)
	if !d.h.exists {
		return "", false, nil
	}
	return "/vms", true, nil
}
func (d fakeReplacer) SetAside(_ context.Context, _ *hypervisor.Connection, name, aside string) error {
	d.h.calls = append(d.h.calls, "aside "+name+" -> "+aside)
	return d.h.asideErr
}
func (d fakeReplacer) PutBack(_ context.Context, _ *hypervisor.Connection, name, aside string) error {
	d.h.calls = append(d.h.calls, "back "+aside+" -> "+name)
	return nil
}

// TestVMReplace follows a virtual machine being restored over the one
// that has its name: that one is set aside only when the disks are
// written, and gets its name back if the new one cannot be made.
func TestVMReplace(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 24, 16, 5, 9, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })

	blobs := map[keys.ID][]byte{}
	put := func(b []byte) (keys.ID, error) {
		id := keys.ID(sha256.Sum256(b))
		blobs[id] = append([]byte(nil), b...)
		return id, nil
	}
	get := func(id keys.ID) ([]byte, error) {
		b, ok := blobs[id]
		if !ok {
			return nil, fmt.Errorf("no blob %s", id.Short())
		}
		return b, nil
	}
	cfg, _ := put([]byte("{}"))
	block, _ := put(make([]byte, 4096))
	pw := tree.NewPageWriter(put)
	pw.Add(&tree.Entry{Name: "fake.vmcfg", Type: tree.TypeFile, Size: 2, Content: []keys.ID{cfg}})
	pw.Add(&tree.Entry{Name: "disk0.img", Type: tree.TypeImage, Size: 8192, BlockSize: 4096, Content: []keys.ID{block, {}}})
	pages, err := pw.Finish()
	if err != nil {
		t.Fatal(err)
	}
	sources := []tree.Entry{{Name: "web01", Type: tree.TypeVM, Subtree: pages}}

	run := func(h *fakeHV, o Options) (warnings []string, err error) {
		var d hypervisor.Driver = fakeDriver{h}
		if h.hasReplace {
			d = fakeReplacer{fakeDriver{h}}
		}
		hypervisor.Register(d)
		o.Sources, o.SourceOS = sources, runtime.GOOS
		o.Warn = func(w string) { warnings = append(warnings, w) }
		_, err = Run(context.Background(), get, o)
		return warnings, err
	}
	const aside = "web01 (replaced 2026-11-24 16.05.09)"

	// The machine is there: it is set aside after the disks, before the
	// new one is made, and the files go beside its own.
	h := &fakeHV{exists: true, hasReplace: true}
	warnings, err := run(h, Options{VMReplace: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"find web01", "new", "disk", "aside web01 -> " + aside, "finish"}
	if !reflect.DeepEqual(h.calls, want) {
		t.Fatalf("calls %q, want %q", h.calls, want)
	}
	if !h.target.Replace || h.target.Place != "/vms" || h.target.Folder != "web01 restored 2026-11-24 16.05.09" || h.target.Name != "web01" {
		t.Fatalf("target %+v", h.target)
	}
	if h.wrote != 4096 {
		t.Fatalf("wrote %d bytes of disk", h.wrote)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], aside) || !strings.Contains(warnings[0], "Delete it") {
		t.Fatalf("warnings %q", warnings)
	}

	// A folder that was asked for wins over the one the machine is in.
	h = &fakeHV{exists: true, hasReplace: true}
	if _, err := run(h, Options{VMReplace: true, Target: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if h.target.Place == "/vms" || h.target.Place == "" {
		t.Fatalf("place %q", h.target.Place)
	}

	// The new machine cannot be made: the old one gets its name back.
	h = &fakeHV{exists: true, hasReplace: true, finishErr: errors.New("no room")}
	_, err = run(h, Options{VMReplace: true})
	if err == nil || !strings.Contains(err.Error(), "no room") || !strings.Contains(err.Error(), "has its name back") {
		t.Fatalf("error %v", err)
	}
	if last := h.calls[len(h.calls)-1]; last != "back "+aside+" -> web01" {
		t.Fatalf("calls %q", h.calls)
	}

	// The old machine cannot be set aside: nothing new is made.
	h = &fakeHV{exists: true, hasReplace: true, asideErr: errors.New("it is locked")}
	_, err = run(h, Options{VMReplace: true})
	if err == nil || !strings.Contains(err.Error(), "it is locked") || !strings.Contains(err.Error(), "left as it is") {
		t.Fatalf("error %v", err)
	}
	if last := h.calls[len(h.calls)-1]; !strings.HasPrefix(last, "aside ") {
		t.Fatalf("calls %q", h.calls)
	}

	// No machine of that name: an ordinary restore, which says so.
	h = &fakeHV{hasReplace: true}
	warnings, err = run(h, Options{VMReplace: true, Target: "/elsewhere", VMName: "web02"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"find web02", "new", "disk", "finish"}; !reflect.DeepEqual(h.calls, want) {
		t.Fatalf("calls %q, want %q", h.calls, want)
	}
	if h.target.Replace || h.target.Folder != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "no machine of that name") {
		t.Fatalf("target %+v, warnings %q", h.target, warnings)
	}

	// Without the option nothing looks for a machine to replace.
	h = &fakeHV{exists: true, hasReplace: true}
	if warnings, err = run(h, Options{Target: "/elsewhere"}); err != nil || len(warnings) != 0 {
		t.Fatal(err, warnings)
	}
	if want := []string{"new", "disk", "finish"}; !reflect.DeepEqual(h.calls, want) {
		t.Fatalf("calls %q, want %q", h.calls, want)
	}

	// A hypervisor that cannot replace says so before anything is done.
	h = &fakeHV{exists: true}
	_, err = run(h, Options{VMReplace: true, Target: "/elsewhere"})
	if err == nil || !strings.Contains(err.Error(), "not built yet for Fake") || len(h.calls) != 0 {
		t.Fatalf("error %v, calls %q", err, h.calls)
	}
}
