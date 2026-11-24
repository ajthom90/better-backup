// Package vmware backs up and restores the virtual machines of VMware
// vSphere: a single ESXi host, or the hosts managed by a vCenter
// Server. Unlike the other hypervisors it is reached over the network,
// so the agent need not run on the host, and cannot: any agent that
// can reach the management address will do.
//
// A machine is read through a snapshot that is exported the way
// vSphere's own "Export OVF template" does it, and rebuilt by
// importing that. Every backup reads the disks in full; storing only
// what changed is left to the repository.
package vmware

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/vmdk"
)

func init() { hypervisor.Register(driver{}) }

type driver struct{}

func (driver) Kind() string       { return "vmware" }
func (driver) Label() string      { return "VMware vSphere" }
func (driver) ConfigFile() string { return "vmware.json" }

// Available is always nil: the host is somewhere else.
func (driver) Available() error { return nil }

// Config is what a backup records about a machine.
type Config struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid,omitempty"`
	State string `json:"state,omitempty"`
	// OVF is vSphere's description of the machine.
	OVF string `json:"ovf"`
	// Files are the small files that belong to the machine, such as
	// its firmware settings, gzipped.
	Files map[string][]byte `json:"files,omitempty"`
	Disks []Disk            `json:"disks"`
	// Where the machine was, by name. A restore goes there again
	// unless told otherwise.
	Server    string `json:"server,omitempty"`
	Datastore string `json:"datastore,omitempty"`
	Host      string `json:"host,omitempty"`
	Pool      string `json:"pool,omitempty"`
	Folder    string `json:"folder,omitempty"`
}

// Disk is one disk of a backed up machine.
type Disk struct {
	// Name is the image's name in the snapshot and the file's name in
	// the OVF description.
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// maxSmallFile bounds the files kept in a Config.
const maxSmallFile = 4 << 20

// snapshotPrefix starts the names of the snapshots this program makes.
const snapshotPrefix = "better-backup "

// conn is a signed-in session.
type conn struct {
	c    *vim25.Client
	sm   *session.Manager
	host string
}

func normalThumbprint(t string) string {
	t = strings.ToUpper(strings.NewReplacer(":", "", " ", "", "-", "").Replace(t))
	var parts []string
	for len(t) >= 2 {
		parts, t = append(parts, t[:2]), t[2:]
	}
	if t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, ":")
}

// seenThumbprint asks a server for its certificate without checking it,
// to be able to say what would have to be trusted.
func seenThumbprint(ctx context.Context, hostport string) string {
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return ""
	}
	defer c.Close()
	certs := c.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return ""
	}
	sum := sha256.Sum256(certs[0].Raw)
	return normalThumbprint(fmt.Sprintf("%x", sum))
}

func connect(ctx context.Context, hc *hypervisor.Connection) (*conn, error) {
	if hc == nil || strings.TrimSpace(hc.URL) == "" {
		return nil, errors.New("no VMware server is set: choose one in the policy, or in the restore")
	}
	u, err := soap.ParseURL(strings.TrimSpace(hc.URL))
	if err != nil || u == nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not the address of a VMware server", hc.URL)
	}
	u.User = url.UserPassword(hc.Username, hc.Password)
	sc := soap.NewClient(u, hc.Insecure)
	if tp := normalThumbprint(hc.Thumbprint); tp != "" && !hc.Insecure {
		sc.SetThumbprint(u.Host, tp)
	}
	fail := func(err error) (*conn, error) {
		if soap.IsCertificateUntrusted(err) || strings.Contains(err.Error(), "thumbprint does not match") {
			hostport := u.Host
			if u.Port() == "" {
				hostport += ":443"
			}
			msg := fmt.Sprintf("the VMware server %s shows a certificate this computer does not trust", u.Hostname())
			if seen := seenThumbprint(ctx, hostport); seen != "" {
				msg += fmt.Sprintf("; if its SHA-256 thumbprint %s is the right one, enter that in the server's settings", seen)
			}
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("connecting to the VMware server %s: %w", u.Hostname(), err)
	}
	vc, err := vim25.NewClient(ctx, sc)
	if err != nil {
		return fail(err)
	}
	sm := session.NewManager(vc)
	if err := sm.Login(ctx, u.User); err != nil {
		if fault.Is(err, &types.InvalidLogin{}) {
			return nil, fmt.Errorf("the VMware server %s does not accept the user name and password", u.Hostname())
		}
		return fail(err)
	}
	return &conn{c: vc, sm: sm, host: u.Hostname()}, nil
}

// close signs out. It works even when the job's context has ended.
func (c *conn) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c.sm.Logout(ctx)
}

// machines lists the server's virtual machines, without templates.
func (c *conn) machines(ctx context.Context, props ...string) ([]mo.VirtualMachine, error) {
	v, err := view.NewManager(c.c).CreateContainerView(ctx, c.c.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		return nil, err
	}
	defer v.Destroy(context.WithoutCancel(ctx))
	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, append([]string{"name", "config.template"}, props...), &vms); err != nil {
		return nil, err
	}
	out := vms[:0]
	for _, vm := range vms {
		if vm.Config != nil && vm.Config.Template {
			continue
		}
		out = append(out, vm)
	}
	return out, nil
}

func (driver) List(ctx context.Context, hc *hypervisor.Connection) ([]string, error) {
	c, err := connect(ctx, hc)
	if err != nil {
		return nil, err
	}
	defer c.close()
	vms, err := c.machines(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, vm := range vms {
		names = append(names, vm.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (c *conn) find(ctx context.Context, name string) (*mo.VirtualMachine, error) {
	vms, err := c.machines(ctx)
	if err != nil {
		return nil, err
	}
	var found []mo.VirtualMachine
	for _, vm := range vms {
		if vm.Name == name {
			found = append(found, vm)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("the VMware server %s has no virtual machine named %q", c.host, name)
	case 1:
	default:
		return nil, fmt.Errorf("the VMware server %s has %d virtual machines named %q; rename them so that each can be told apart", c.host, len(found), name)
	}
	var vm mo.VirtualMachine
	err = property.DefaultCollector(c.c).RetrieveOne(ctx, found[0].Self,
		[]string{"name", "config", "runtime", "snapshot", "resourcePool", "parent", "guest.toolsRunningStatus"}, &vm)
	if err != nil {
		return nil, err
	}
	return &vm, nil
}

// wait waits for a task the server was given.
func (c *conn) wait(ctx context.Context, ref types.ManagedObjectReference) (*types.TaskInfo, error) {
	return object.NewTask(c.c, ref).WaitForResultEx(ctx)
}

func (c *conn) removeSnapshot(ctx context.Context, ref types.ManagedObjectReference) error {
	res, err := methods.RemoveSnapshot_Task(ctx, c.c, &types.RemoveSnapshot_Task{This: ref, RemoveChildren: false, Consolidate: types.NewBool(true)})
	if err != nil {
		return err
	}
	_, err = c.wait(ctx, res.Returnval)
	return err
}

func staleSnapshots(tree []types.VirtualMachineSnapshotTree, prefix string, out *[]types.ManagedObjectReference) {
	for _, s := range tree {
		// Children first: removing a snapshot hands its changes to
		// its parent.
		staleSnapshots(s.ChildSnapshotList, prefix, out)
		if strings.HasPrefix(s.Name, prefix) {
			*out = append(*out, s.Snapshot)
		}
	}
}

// keepalive tells the server at intervals that a lease is still in
// use; it would end the lease after a few minutes otherwise.
type keepalive struct {
	stop    chan struct{}
	stopped sync.WaitGroup
	mu      sync.Mutex
	err     error
	done    int64
	total   int64
}

func startKeepalive(lease *nfc.Lease, timeout int32, total int64) *keepalive {
	every := time.Duration(timeout) * time.Second / 4
	every = min(max(every, 2*time.Second), 30*time.Second)
	k := &keepalive{stop: make(chan struct{}), total: total}
	k.stopped.Add(1)
	go func() {
		defer k.stopped.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-t.C:
			}
			k.mu.Lock()
			percent := int32(0)
			if k.total > 0 {
				percent = int32(min(99, k.done*100/k.total))
			}
			k.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := lease.Progress(ctx, percent)
			cancel()
			if err != nil {
				k.mu.Lock()
				k.err = err
				k.mu.Unlock()
				return
			}
		}
	}()
	return k
}

func (k *keepalive) add(n int64) {
	k.mu.Lock()
	k.done += n
	k.mu.Unlock()
}

func (k *keepalive) end() error {
	select {
	case <-k.stop:
	default:
		close(k.stop)
	}
	k.stopped.Wait()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// stream reads one exported disk. The download starts with the first
// read, so that a machine's later disks do not sit idle on the wire
// while the earlier ones are read.
type stream struct {
	name string
	size int64
	open func() (io.ReadCloser, error)
	seen func(int64)

	body io.ReadCloser
	r    *vmdk.Reader
	err  error
	read int64
	// whole says the disk was read to its end and the stream ended
	// the way the format says it must.
	whole bool
}

func (s *stream) start() error {
	if s.r != nil || s.err != nil {
		return s.err
	}
	if s.whole {
		return errors.New("the download has ended")
	}
	body, err := s.open()
	if err != nil {
		s.err = fmt.Errorf("downloading %s: %w", s.name, err)
		return s.err
	}
	r, err := vmdk.NewReader(body)
	if err != nil {
		body.Close()
		s.err = fmt.Errorf("downloading %s: %w", s.name, err)
		return s.err
	}
	if s.size != 0 && r.Size() != s.size {
		body.Close()
		s.err = fmt.Errorf("the server sent a disk of %d bytes for %s, which should have %d", r.Size(), s.name, s.size)
		return s.err
	}
	s.body, s.r = body, r
	return nil
}

func (s *stream) ReadAt(p []byte, off int64) (int, error) {
	if err := s.start(); err != nil {
		return 0, err
	}
	n, err := s.r.ReadAt(p, off)
	if err != nil && err != io.EOF {
		s.err = fmt.Errorf("downloading %s: %w", s.name, err)
		return n, s.err
	}
	if end := off + int64(n); end > s.read {
		if s.seen != nil {
			s.seen(end - s.read)
		}
		s.read = end
	}
	if s.read == s.size && !s.whole {
		// What follows the last block says whether the stream is
		// complete. It is read now, while the download is open.
		if derr := s.r.Drain(); derr != nil {
			s.err = fmt.Errorf("downloading %s: %w", s.name, derr)
			return n, s.err
		}
		s.whole = true
	}
	return n, err
}

// finish says whether the whole disk arrived.
func (s *stream) finish() error {
	if s.err != nil {
		return s.err
	}
	if !s.whole {
		return fmt.Errorf("the disk %s was not read to its end", s.name)
	}
	return nil
}

// close ends the download. Nothing can be read after it.
func (s *stream) close() {
	if s.body != nil {
		s.body.Close()
		s.body = nil
	}
	if s.err == nil && !s.whole {
		s.err = fmt.Errorf("the download of %s was ended early", s.name)
	}
	s.r = nil
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(r, maxSmallFile+1))
}

// notExported names the disks of a machine that a snapshot, and so a
// backup, leaves out.
func notExported(devices []types.BaseVirtualDevice) (warnings []string) {
	for _, dev := range devices {
		d, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		label := "a disk"
		if d.DeviceInfo != nil && d.DeviceInfo.GetDescription().Label != "" {
			label = d.DeviceInfo.GetDescription().Label
		}
		mode := ""
		switch b := d.Backing.(type) {
		case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
			if b.CompatibilityMode == string(types.VirtualDiskCompatibilityModePhysicalMode) {
				warnings = append(warnings, label+" is a disk of the storage network handed to the machine as it is (a physical mode RDM) and was not backed up")
				continue
			}
			mode = b.DiskMode
		case *types.VirtualDiskFlatVer2BackingInfo:
			mode = b.DiskMode
		case *types.VirtualDiskSeSparseBackingInfo:
			mode = b.DiskMode
		case *types.VirtualDiskSparseVer2BackingInfo:
			mode = b.DiskMode
		}
		if strings.HasPrefix(mode, "independent") {
			warnings = append(warnings, label+" is an independent disk, which snapshots leave out, and was not backed up")
		}
	}
	return warnings
}

func (driver) Freeze(ctx context.Context, name string, o hypervisor.Options) (fz *hypervisor.Frozen, err error) {
	c, err := connect(ctx, o.Connection)
	if err != nil {
		return nil, err
	}
	// What is set up so far is taken down again if a later step fails.
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
			c.close()
		}
	}()
	vm, err := c.find(ctx, name)
	if err != nil {
		return nil, err
	}
	if vm.Config == nil {
		return nil, errors.New("the server does not show its configuration; it may be unreachable on its host")
	}
	fz = &hypervisor.Frozen{}
	fz.Warnings = notExported(vm.Config.Hardware.Device)

	// Snapshots an interrupted backup of this policy left behind.
	prefix := snapshotPrefix + o.Scope + " "
	if vm.Snapshot != nil {
		var stale []types.ManagedObjectReference
		staleSnapshots(vm.Snapshot.RootSnapshotList, prefix, &stale)
		for _, ref := range stale {
			if rerr := c.removeSnapshot(ctx, ref); rerr != nil {
				fz.Warnings = append(fz.Warnings, "a snapshot left by an earlier backup could not be removed: "+rerr.Error())
			}
		}
	}

	on := vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
	quiesce := on && vm.Guest != nil && vm.Guest.ToolsRunningStatus == string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
	snapshot := func(quiesce bool) (types.ManagedObjectReference, error) {
		res, err := methods.CreateSnapshot_Task(ctx, c.c, &types.CreateSnapshot_Task{This: vm.Self, Name: prefix + o.Tag,
			Description: "Made by better-backup to read the machine's disks. It is removed when the backup ends.", Memory: false, Quiesce: quiesce})
		if err != nil {
			return types.ManagedObjectReference{}, err
		}
		info, err := c.wait(ctx, res.Returnval)
		if err != nil {
			return types.ManagedObjectReference{}, err
		}
		ref, ok := info.Result.(types.ManagedObjectReference)
		if !ok {
			return types.ManagedObjectReference{}, errors.New("the server did not say which snapshot it made")
		}
		return ref, nil
	}
	snap, err := snapshot(quiesce)
	if err != nil && quiesce && ctx.Err() == nil {
		var qerr error
		if snap, qerr = snapshot(false); qerr == nil {
			fz.Warnings = append(fz.Warnings, "its guest could not be quiesced ("+err.Error()+"); the backup is what its disks would hold after a power cut")
			err = nil
		}
	}
	if err != nil {
		if fault.Is(err, &types.RestrictedVersion{}) || strings.Contains(err.Error(), "license") {
			return nil, fmt.Errorf("taking a snapshot: %w (the free ESXi license does not let other programs make backups)", err)
		}
		return nil, fmt.Errorf("taking a snapshot: %w", err)
	}
	removed := false
	remove := func() error {
		if removed {
			return nil
		}
		removed = true
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := c.removeSnapshot(rctx, snap); err != nil {
			return fmt.Errorf("the snapshot %q made for this backup could not be removed; remove it in vSphere: %w", prefix+o.Tag, err)
		}
		return nil
	}
	undo = append(undo, func() { remove() })

	res, err := methods.ExportSnapshot(ctx, c.c, &types.ExportSnapshot{This: snap})
	if err != nil {
		return nil, fmt.Errorf("asking for the snapshot's disks: %w", err)
	}
	lease := nfc.NewLease(c.c, res.Returnval)
	info, err := lease.Wait(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("asking for the snapshot's disks: %w", err)
	}
	leaseDone := false
	endLease := func(ok bool) error {
		if leaseDone {
			return nil
		}
		leaseDone = true
		lctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if ok {
			return lease.Complete(lctx)
		}
		return lease.Abort(lctx, nil)
	}
	undo = append(undo, func() { endLease(false) })
	if len(info.Items) != len(info.DeviceUrl) {
		return nil, errors.New("the server's list of files to download is not understood")
	}

	cfg := Config{Name: vm.Name, UUID: vm.Config.Uuid, State: string(vm.Runtime.PowerState), Server: c.host, Files: map[string][]byte{}}
	var files []types.OvfFile
	var streams []*stream
	for i, item := range info.Items {
		dev := info.DeviceUrl[i]
		u := item.URL
		open := func() (io.ReadCloser, error) {
			body, _, err := c.c.Download(ctx, u, &soap.Download{Method: "GET"})
			return body, err
		}
		if dev.Disk != nil && *dev.Disk {
			name := fmt.Sprintf("disk-%d.vmdk", len(streams))
			streams = append(streams, &stream{name: name, open: open})
			files = append(files, types.OvfFile{DeviceId: dev.Key, Path: name, Size: 1})
			continue
		}
		// Anything else that is small belongs to the machine, such as
		// its firmware settings. Disc images do not.
		target := strings.ToLower(dev.TargetId)
		if strings.HasSuffix(target, ".iso") || strings.HasSuffix(target, ".flp") || dev.FileSize > maxSmallFile {
			continue
		}
		body, err := open()
		if err != nil {
			fz.Warnings = append(fz.Warnings, fmt.Sprintf("its file %s could not be downloaded and is not in the backup: %v", dev.TargetId, err))
			continue
		}
		b, err := io.ReadAll(io.LimitReader(body, maxSmallFile+1))
		body.Close()
		if err != nil || len(b) > maxSmallFile {
			if err != nil {
				fz.Warnings = append(fz.Warnings, fmt.Sprintf("its file %s could not be downloaded and is not in the backup: %v", dev.TargetId, err))
			}
			continue
		}
		name := hypervisor.SafeName(dev.TargetId)
		if name == "" || cfg.Files[name] != nil {
			name = fmt.Sprintf("file-%d", len(cfg.Files))
		}
		cfg.Files[name] = gz(b)
		files = append(files, types.OvfFile{DeviceId: dev.Key, Path: name, Size: int64(len(b))})
	}
	if len(streams) == 0 {
		return nil, errors.New("it has no disk that can be backed up")
	}

	// The description names the files above. It is asked for now,
	// while the snapshot is what the machine looks like.
	params := types.OvfCreateDescriptorParams{Name: vm.Name, OvfFiles: files}
	if len(cfg.Files) > 0 {
		var m mo.OvfManager
		if c.c.ServiceContent.OvfManager != nil &&
			property.DefaultCollector(c.c).RetrieveOne(ctx, *c.c.ServiceContent.OvfManager, []string{"ovfExportOption"}, &m) == nil {
			for _, opt := range m.OvfExportOption {
				if opt.Option == "nvram" {
					params.ExportOption = append(params.ExportOption, opt.Option)
				}
			}
		}
	}
	desc, err := ovf.NewManager(c.c).CreateDescriptor(ctx, vm.Self, params)
	if err != nil {
		return nil, fmt.Errorf("asking for the machine's description: %w", err)
	}
	for _, f := range desc.Error {
		return nil, fmt.Errorf("the server could not describe the machine: %s", f.LocalizedMessage)
	}
	cfg.OVF = desc.OvfDescriptor
	parsed, err := parseDescriptor(cfg.OVF)
	if err != nil {
		return nil, err
	}

	var total int64
	for _, s := range streams {
		s.size = parsed.Capacity[s.name]
		if s.size == 0 {
			// The description does not say: the disk itself does.
			if err := s.start(); err != nil {
				return nil, err
			}
			s.size = s.r.Size()
			if s != streams[0] {
				s.body.Close()
				s.body, s.r = nil, nil
			}
		}
		if s.size <= 0 {
			return nil, fmt.Errorf("the server did not say how large the disk %s is", s.name)
		}
		total += s.size
		cfg.Disks = append(cfg.Disks, Disk{Name: s.name, Size: s.size})
	}
	alive := startKeepalive(lease, info.LeaseTimeout, total)
	undo = append(undo, func() { alive.end() })
	for _, s := range streams {
		s.seen = alive.add
		fz.Disks = append(fz.Disks, backup.ImageSource{Name: s.name, Size: s.size, R: s, SkipZero: true, Close: s.close})
	}

	// Where it is, for a restore to the same place.
	if path, perr := find.InventoryPath(ctx, c.c, vm.Self); perr == nil {
		if i := strings.LastIndex(path, "/"); i > 0 {
			cfg.Folder = path[:i]
		}
	}
	if vm.ResourcePool != nil {
		cfg.Pool, _ = find.InventoryPath(ctx, c.c, *vm.ResourcePool)
	}
	if vm.Runtime.Host != nil {
		var h mo.HostSystem
		if property.DefaultCollector(c.c).RetrieveOne(ctx, *vm.Runtime.Host, []string{"name"}, &h) == nil {
			cfg.Host = h.Name
		}
	}
	if vm.Config.Files.VmPathName != "" {
		var p object.DatastorePath
		if p.FromString(vm.Config.Files.VmPathName) {
			cfg.Datastore = p.Datastore
		}
	}
	if fz.Config, err = json.Marshal(cfg); err != nil {
		return nil, err
	}

	for i, w := range fz.Warnings {
		fz.Warnings[i] = vm.Name + ": " + w
	}

	fz.Check = func() error {
		for _, s := range streams {
			if err := s.finish(); err != nil {
				return err
			}
		}
		if err := alive.end(); err != nil {
			return fmt.Errorf("the server ended the download early: %w", err)
		}
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if l, err := lease.Properties(cctx, "state", "error"); err == nil && l.State == types.HttpNfcLeaseStateError {
			msg := "the server reports an error for the download"
			if l.Error != nil {
				msg += ": " + l.Error.LocalizedMessage
			}
			return errors.New(msg)
		}
		return nil
	}
	fz.Release = func() error {
		ok := true
		for _, s := range streams {
			ok = ok && s.whole
			s.close()
		}
		alive.end()
		endLease(ok)
		err := remove()
		c.close()
		return err
	}
	return fz, nil
}
