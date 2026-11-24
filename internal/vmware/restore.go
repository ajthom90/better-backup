package vmware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/vmdk"
)

// place is where a restored machine goes.
type place struct {
	datastore types.ManagedObjectReference
	dsName    string
	host      types.ManagedObjectReference
	pool      types.ManagedObjectReference
	folder    *types.ManagedObjectReference
	networks  map[string]types.ManagedObjectReference
	anyNet    *types.ManagedObjectReference
}

func (c *conn) retrieve(ctx context.Context, kind string, props []string, dst any) error {
	v, err := view.NewManager(c.c).CreateContainerView(ctx, c.c.ServiceContent.RootFolder, []string{kind}, true)
	if err != nil {
		return err
	}
	defer v.Destroy(context.WithoutCancel(ctx))
	return v.Retrieve(ctx, []string{kind}, props, dst)
}

// locate works out where a machine goes: the datastore that was asked
// for, or else the one it was on; a host that has that datastore, the
// one it ran on if possible; and its old folder and resource pool if
// they are still there and go with that host.
func (c *conn) locate(ctx context.Context, cfg *Config, want string) (*place, error) {
	pc := property.DefaultCollector(c.c)
	index := object.NewSearchIndex(c.c)
	var stores []mo.Datastore
	if err := c.retrieve(ctx, "Datastore", []string{"name", "host", "summary.accessible"}, &stores); err != nil {
		return nil, err
	}
	name := want
	if name == "" {
		name = cfg.Datastore
	}
	var ds *mo.Datastore
	if strings.Contains(name, "/") {
		ref, err := index.FindByInventoryPath(ctx, name)
		if err != nil {
			return nil, err
		}
		for i := range stores {
			if ref != nil && stores[i].Self == ref.Reference() {
				ds = &stores[i]
			}
		}
	} else {
		var found []*mo.Datastore
		for i := range stores {
			if stores[i].Name == name {
				found = append(found, &stores[i])
			}
		}
		if len(found) > 1 {
			return nil, fmt.Errorf("the VMware server %s has %d datastores named %q; give the full path of one, such as /Datacenter/datastore/%s", c.host, len(found), name, name)
		}
		if len(found) == 1 {
			ds = found[0]
		}
	}
	if ds == nil {
		var names []string
		for _, s := range stores {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		if name == "" {
			return nil, fmt.Errorf("say which datastore the machine goes to: %s", strings.Join(names, ", "))
		}
		return nil, fmt.Errorf("the VMware server %s has no datastore %q; it has: %s", c.host, name, strings.Join(names, ", "))
	}
	if !ds.Summary.Accessible {
		return nil, fmt.Errorf("the datastore %s cannot be reached by its hosts right now", ds.Name)
	}
	p := &place{datastore: ds.Self, dsName: ds.Name, networks: map[string]types.ManagedObjectReference{}}

	// A host that has the datastore.
	var refs []types.ManagedObjectReference
	for _, m := range ds.Host {
		if m.MountInfo.Mounted == nil || *m.MountInfo.Mounted {
			refs = append(refs, m.Key)
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no host has the datastore %s mounted", ds.Name)
	}
	var hosts []mo.HostSystem
	if err := pc.Retrieve(ctx, refs, []string{"name", "parent", "network", "runtime.connectionState", "runtime.inMaintenanceMode"}, &hosts); err != nil {
		return nil, err
	}
	var host *mo.HostSystem
	for i := range hosts {
		h := &hosts[i]
		if h.Runtime.ConnectionState != types.HostSystemConnectionStateConnected || h.Runtime.InMaintenanceMode {
			continue
		}
		if host == nil || h.Name == cfg.Host {
			host = h
		}
	}
	if host == nil || host.Parent == nil {
		return nil, fmt.Errorf("no host that has the datastore %s is connected and out of maintenance mode", ds.Name)
	}
	p.host = host.Self

	// Its resource pool: the old one if it belongs to this host's
	// cluster, or else the cluster's own.
	var cr mo.ComputeResource
	if err := pc.RetrieveOne(ctx, *host.Parent, []string{"resourcePool"}, &cr); err != nil {
		return nil, err
	}
	if cr.ResourcePool == nil {
		return nil, fmt.Errorf("the host %s has no resource pool", host.Name)
	}
	p.pool = *cr.ResourcePool
	if cfg.Pool != "" {
		if ref, err := index.FindByInventoryPath(ctx, cfg.Pool); err == nil && ref != nil && ref.Reference().Type == "ResourcePool" {
			var rp mo.ResourcePool
			if pc.RetrieveOne(ctx, ref.Reference(), []string{"owner"}, &rp) == nil && rp.Owner == *host.Parent {
				p.pool = ref.Reference()
			}
		}
	}

	// Its folder, if that is in the datastore's datacenter. Without
	// one the server uses the datacenter's own.
	if cfg.Folder != "" {
		if ref, err := index.FindByInventoryPath(ctx, cfg.Folder); err == nil && ref != nil && ref.Reference().Type == "Folder" {
			up, err1 := mo.Ancestors(ctx, c.c, c.c.ServiceContent.PropertyCollector, ref.Reference())
			dsUp, err2 := mo.Ancestors(ctx, c.c, c.c.ServiceContent.PropertyCollector, ds.Self)
			dc := func(es []mo.ManagedEntity) string {
				for _, e := range es {
					if e.Self.Type == "Datacenter" {
						return e.Self.Value
					}
				}
				return ""
			}
			if err1 == nil && err2 == nil && dc(up) != "" && dc(up) == dc(dsUp) {
				r := ref.Reference()
				p.folder = &r
			}
		}
	}

	if len(host.Network) > 0 {
		var nets []mo.Network
		if err := pc.Retrieve(ctx, host.Network, []string{"name"}, &nets); err != nil {
			return nil, err
		}
		sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
		for i, n := range nets {
			if i == 0 {
				r := n.Self
				p.anyNet = &r
			}
			if _, dup := p.networks[n.Name]; !dup {
				p.networks[n.Name] = n.Self
			}
		}
	}
	return p, nil
}

// dropFileSizes takes the sizes of the files out of a description.
// They are those of the files as they were downloaded; what is
// uploaded holds the same disks but is not the same length, and the
// sizes are optional.
func dropFileSizes(doc string) string {
	return fileElement.ReplaceAllStringFunc(doc, func(el string) string {
		return sizeAttr.ReplaceAllString(el, "")
	})
}

type restore struct {
	ctx      context.Context
	c        *conn
	cfg      Config
	name     string
	place    *place
	spec     *types.OvfCreateImportSpecResult
	lease    *nfc.Lease
	info     *nfc.LeaseInfo
	alive    *keepalive
	sent     map[string]bool
	warnings []string
	closed   bool
}

func (driver) NewRestore(ctx context.Context, config []byte, t hypervisor.Target) (_ hypervisor.Restore, err error) {
	var cfg Config
	if err := json.Unmarshal(config, &cfg); err != nil || cfg.OVF == "" {
		return nil, errors.New("the backup's description of the machine cannot be read")
	}
	c, err := connect(ctx, t.Connection)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.close()
		}
	}()
	r := &restore{ctx: ctx, c: c, cfg: cfg, name: t.Name, sent: map[string]bool{}}
	if r.name == "" {
		r.name = cfg.Name
	}
	vms, err := c.machines(ctx)
	if err != nil {
		return nil, err
	}
	for _, vm := range vms {
		if vm.Name == r.name {
			return nil, fmt.Errorf("the VMware server %s already has a virtual machine named %q; give the restored one another name", c.host, r.name)
		}
	}
	if r.place, err = c.locate(ctx, &cfg, t.Place); err != nil {
		return nil, err
	}
	doc := dropFileSizes(cfg.OVF)
	parsed, err := parseDescriptor(doc)
	if err != nil {
		return nil, err
	}
	params := types.OvfCreateImportSpecParams{
		OvfManagerCommonParams: types.OvfManagerCommonParams{Locale: "US"},
		EntityName:             r.name,
		HostSystem:             &r.place.host,
		DiskProvisioning:       string(types.OvfCreateImportSpecParamsDiskProvisioningTypeThin),
	}
	for _, n := range parsed.Networks {
		ref, ok := r.place.networks[n]
		if !ok {
			if r.place.anyNet == nil {
				r.warnings = append(r.warnings, fmt.Sprintf("its network %q does not exist on the host; connect its network adapter by hand", n))
				continue
			}
			ref = *r.place.anyNet
			for name, nref := range r.place.networks {
				if nref == ref {
					r.warnings = append(r.warnings, fmt.Sprintf("its network %q does not exist on the host; its network adapter was connected to %q instead", n, name))
				}
			}
		}
		params.NetworkMapping = append(params.NetworkMapping, types.OvfNetworkMapping{Name: n, Network: ref})
	}
	r.spec, err = ovf.NewManager(c.c).CreateImportSpec(ctx, doc, r.place.pool, r.place.datastore, &params)
	if err != nil {
		return nil, fmt.Errorf("the server does not accept the machine's description: %w", err)
	}
	if len(r.spec.Error) > 0 || r.spec.ImportSpec == nil {
		var msgs []string
		for _, f := range r.spec.Error {
			msgs = append(msgs, f.LocalizedMessage)
		}
		return nil, fmt.Errorf("the server does not accept the machine's description: %s", strings.Join(msgs, "; "))
	}
	for _, f := range r.spec.Warning {
		r.warnings = append(r.warnings, "the VMware server says: "+f.LocalizedMessage)
	}
	return r, nil
}

func (r *restore) Where() string {
	return "the datastore " + r.place.dsName + " of " + r.c.host
}

// begin asks the server to make the machine and to take its files.
func (r *restore) begin() error {
	if r.lease != nil {
		return nil
	}
	var folder *object.Folder
	if r.place.folder != nil {
		folder = object.NewFolder(r.c.c, *r.place.folder)
	}
	host := object.NewHostSystem(r.c.c, r.place.host)
	lease, err := object.NewResourcePool(r.c.c, r.place.pool).ImportVApp(r.ctx, r.spec.ImportSpec, folder, host)
	if err != nil {
		return fmt.Errorf("creating the machine: %w", err)
	}
	info, err := lease.Wait(r.ctx, r.spec.FileItem)
	if err != nil {
		return fmt.Errorf("creating the machine: %w", err)
	}
	var total int64
	for _, d := range r.cfg.Disks {
		total += d.Size
	}
	r.lease, r.info = lease, info
	r.alive = startKeepalive(lease, info.LeaseTimeout, total)
	return nil
}

func (r *restore) item(name string) *nfc.FileItem {
	for i := range r.info.Items {
		if r.info.Items[i].Path == name {
			return &r.info.Items[i]
		}
	}
	return nil
}

// counted tells the keepalive how far a disk has come.
type counted struct {
	w     io.WriterAt
	alive *keepalive
}

func (c counted) WriteAt(p []byte, off int64) (int, error) {
	n, err := c.w.WriteAt(p, off)
	c.alive.add(int64(n))
	return n, err
}

func (r *restore) Disk(d hypervisor.DiskSpec) (io.WriterAt, bool, func() error, error) {
	if err := r.begin(); err != nil {
		return nil, false, nil, err
	}
	item := r.item(d.Name)
	if item == nil || r.sent[d.Name] {
		return nil, false, nil, errors.New("the machine's description has no place for this disk")
	}
	size, err := vmdk.StreamSize(d.Size, d.BlockSize, d.Data)
	if err != nil {
		return nil, false, nil, err
	}
	pr, pw := io.Pipe()
	result := make(chan error, 1)
	go func() {
		err := r.c.c.Upload(r.ctx, pr, item.URL, &soap.Upload{Method: "POST", Type: "application/x-vnd.vmware-streamVmdk", ContentLength: size})
		// Whatever is still being written has nowhere to go now.
		pr.CloseWithError(errors.Join(errors.New("the upload ended"), err))
		result <- err
	}()
	w, err := vmdk.NewWriter(pw, d.Size, d.BlockSize, d.Data)
	if err != nil {
		pw.CloseWithError(err)
		<-result
		return nil, false, nil, err
	}
	done := func() error {
		cerr := w.Close()
		pw.CloseWithError(cerr)
		uerr := <-result
		if uerr != nil {
			return fmt.Errorf("uploading the disk: %w", uerr)
		}
		if cerr != nil {
			return cerr
		}
		r.sent[d.Name] = true
		return nil
	}
	// The server makes the disk; what is not uploaded reads as zeros.
	return counted{w, r.alive}, true, done, nil
}

func (r *restore) Finish() ([]string, error) {
	if err := r.begin(); err != nil {
		r.close()
		return nil, err
	}
	fail := func(err error) ([]string, error) {
		r.Abort()
		return nil, err
	}
	for _, item := range r.info.Items {
		if r.sent[item.Path] {
			continue
		}
		packed, ok := r.cfg.Files[item.Path]
		if !ok {
			for _, d := range r.cfg.Disks {
				if d.Name == item.Path {
					return fail(fmt.Errorf("the backup does not hold the machine's disk %s", d.Name))
				}
			}
			r.warnings = append(r.warnings, fmt.Sprintf("its file %s is not in the backup", item.Path))
			continue
		}
		b, err := gunzip(packed)
		if err != nil {
			return fail(fmt.Errorf("the backup's copy of the machine's file %s cannot be read: %w", item.Path, err))
		}
		up := soap.Upload{Method: "POST", Type: "application/octet-stream", ContentLength: int64(len(b))}
		if item.Create {
			up.Method, up.Headers = "PUT", map[string]string{"Overwrite": "t"}
		}
		if err := r.c.c.Upload(r.ctx, bytes.NewReader(b), item.URL, &up); err != nil {
			return fail(fmt.Errorf("uploading the machine's file %s: %w", item.Path, err))
		}
	}
	if err := r.alive.end(); err != nil {
		return fail(fmt.Errorf("the server ended the upload early: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Minute)
	defer cancel()
	if err := r.lease.Complete(ctx); err != nil {
		return fail(fmt.Errorf("the server did not accept the uploaded machine: %w", err))
	}
	r.close()
	if strings.Contains(r.cfg.OVF, "<rasd:Connection>") {
		r.warnings = append(r.warnings, "its network adapters have new MAC addresses")
	}
	return r.warnings, nil
}

// Abort gives up on a machine that is half made; the server then
// removes what there is of it.
func (r *restore) Abort() {
	if r.closed {
		return
	}
	if r.lease != nil {
		r.alive.end()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r.lease.Abort(ctx, nil)
		cancel()
	}
	r.close()
}

func (r *restore) close() {
	if !r.closed {
		r.closed = true
		r.c.close()
	}
}
