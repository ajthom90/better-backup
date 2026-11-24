// Package vmwaretest runs govmomi's vSphere simulator for tests, with
// what the simulator lacks for backups put in: disks that hold data,
// descriptions of machines, and a record of what was uploaded.
package vmwaretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/vmdk"
)

// Block is the size of the blocks disks are planted and read in.
const Block = 1 << 20

// Sim is a running simulator of a vCenter Server with a few hosts and
// machines, such as DC0_H0_VM0.
type Sim struct {
	// Addr is the server's host:port.
	Addr       string
	Username   string
	Password   string
	Thumbprint string

	model *simulator.Model
	mu    sync.Mutex
	// uploads holds the bodies of the files uploaded for imports, by
	// file name, newest last.
	uploads map[string][][]byte
	sizes   map[string][]int64
	url     *url.URL
}

// Start runs a simulator until the test ends.
func Start(t testing.TB) *Sim {
	t.Helper()
	m := simulator.VPX()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	s := &Sim{model: m, uploads: map[string][][]byte{}, sizes: map[string][]int64{}}
	m.Service.TLS = new(tls.Config)
	m.Service.ServeMux = http.NewServeMux()
	// Uploads are kept before the simulator, which only counts them,
	// sees them.
	m.Service.ServeMux.HandleFunc("POST /nfc/", s.upload)
	m.Service.ServeMux.HandleFunc("PUT /nfc/", s.upload)
	srv := m.Service.NewServer()
	t.Cleanup(func() {
		srv.Close()
		m.Remove()
	})
	ctx := m.Service.Context
	old := ctx.Map.Get(*ctx.Map.Get(types.ManagedObjectReference{Type: "ServiceInstance", Value: "ServiceInstance"}).(*simulator.ServiceInstance).Content.OvfManager)
	ctx.Map.Put(&ovfManager{OvfManager: old.(*simulator.OvfManager), sim: s})
	s.Addr, s.url = srv.URL.Host, srv.URL
	s.Username = srv.URL.User.Username()
	s.Password, _ = srv.URL.User.Password()
	sum := sha256.Sum256(srv.Certificate().Raw)
	s.Thumbprint = fmt.Sprintf("%X", sum)
	return s
}

// Connection is how a driver reaches the simulator.
func (s *Sim) Connection() *hypervisor.Connection {
	return &hypervisor.Connection{URL: s.Addr, Username: s.Username, Password: s.Password, Thumbprint: s.Thumbprint}
}

func (s *Sim) upload(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.ContentLength != int64(len(body)) {
		http.Error(w, "the upload did not say how long it is", http.StatusLengthRequired)
		return
	}
	name := path.Base(r.URL.Path)
	s.mu.Lock()
	s.uploads[name] = append(s.uploads[name], body)
	s.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	simulator.ServeNFC(w, r)
}

// Uploaded returns the disk last uploaded under a file name, as the
// contents a machine would read from it.
func (s *Sim) Uploaded(t testing.TB, name string) []byte {
	t.Helper()
	s.mu.Lock()
	list := s.uploads[name]
	s.mu.Unlock()
	if len(list) == 0 {
		t.Fatalf("nothing was uploaded as %s", name)
	}
	r, err := vmdk.NewReader(bytes.NewReader(list[len(list)-1]))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, r.Size())
	if _, err := r.ReadAt(out, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *Sim) vm(name string) *simulator.VirtualMachine {
	for _, e := range s.model.Service.Context.Map.All("VirtualMachine") {
		if vm := e.(*simulator.VirtualMachine); vm.Name == name {
			return vm
		}
	}
	return nil
}

// Machines names the simulator's machines.
func (s *Sim) Machines() []string {
	var out []string
	for _, e := range s.model.Service.Context.Map.All("VirtualMachine") {
		out = append(out, e.(*simulator.VirtualMachine).Name)
	}
	return out
}

// Snapshots names the snapshots a machine has.
func (s *Sim) Snapshots(name string) []string {
	vm := s.vm(name)
	if vm == nil || vm.Snapshot == nil {
		return nil
	}
	var out []string
	var walk func([]types.VirtualMachineSnapshotTree)
	walk = func(list []types.VirtualMachineSnapshotTree) {
		for _, n := range list {
			out = append(out, n.Name)
			walk(n.ChildSnapshotList)
		}
	}
	walk(vm.Snapshot.RootSnapshotList)
	return out
}

// diskFiles lists where the simulator keeps a machine's disks.
func (s *Sim) diskFiles(t testing.TB, name string) []string {
	t.Helper()
	vm := s.vm(name)
	if vm == nil {
		t.Fatalf("the simulator has no machine %s", name)
	}
	var out []string
	for _, dev := range vm.Config.Hardware.Device {
		d, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		b, ok := d.Backing.(types.BaseVirtualDeviceFileBackingInfo)
		if !ok {
			continue
		}
		var p object.DatastorePath
		p.FromString(b.GetVirtualDeviceFileBackingInfo().FileName)
		for _, e := range s.model.Service.Context.Map.All("Datastore") {
			if ds := e.(*simulator.Datastore); ds.Name == p.Datastore {
				out = append(out, filepath.Join(ds.Info.GetDatastoreInfo().Url, p.Path))
			}
		}
	}
	return out
}

// Plant gives a machine's disks contents, one disk for each argument:
// the simulator then serves them, in VMware's export format, when the
// disks are exported. Blocks of zeros are left out the way a thin disk
// leaves them out. A machine with too few disks gets more.
func (s *Sim) Plant(t testing.TB, name string, disks ...[]byte) {
	t.Helper()
	for len(s.diskFiles(t, name)) < len(disks) {
		s.addDisk(t, name)
	}
	files := s.diskFiles(t, name)
	var sizes []int64
	for n, contents := range disks {
		data := make([]bool, (len(contents)+Block-1)/Block)
		for i := range data {
			blk := contents[i*Block : min(len(contents), (i+1)*Block)]
			data[i] = len(bytes.Trim(blk, "\x00")) > 0
		}
		var buf bytes.Buffer
		w, err := vmdk.NewWriter(&buf, int64(len(contents)), Block, data)
		if err != nil {
			t.Fatal(err)
		}
		for i, has := range data {
			if has {
				if _, err := w.WriteAt(contents[i*Block:min(len(contents), (i+1)*Block)], int64(i)*Block); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(files[n], buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, int64(len(contents)))
	}
	s.mu.Lock()
	s.sizes[name] = sizes
	s.mu.Unlock()
}

func (s *Sim) addDisk(t testing.TB, name string) {
	t.Helper()
	ctx := context.Background()
	c, err := govmomi.NewClient(ctx, s.url, true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout(ctx)
	vm := object.NewVirtualMachine(c.Client, s.vm(name).Self)
	devices, err := vm.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := devices.FindDiskController("")
	if err != nil {
		t.Fatal(err)
	}
	var first *types.VirtualDisk
	for _, d := range devices.SelectByType((*types.VirtualDisk)(nil)) {
		first = d.(*types.VirtualDisk)
		break
	}
	var p object.DatastorePath
	p.FromString(first.Backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo().FileName)
	p.Path = path.Join(path.Dir(p.Path), fmt.Sprintf("more-%d.vmdk", len(devices)))
	disk := devices.CreateDisk(controller, first.Backing.(*types.VirtualDiskFlatVer2BackingInfo).Datastore.Reference(), p.String())
	disk.CapacityInKB = 1024
	if err := vm.AddDevice(ctx, disk); err != nil {
		t.Fatal(err)
	}
}

// Truncate cuts the stored export of a machine's first disk short, as
// a download that breaks off would.
func (s *Sim) Truncate(t testing.TB, name string, keep int64) {
	t.Helper()
	if err := os.Truncate(s.diskFiles(t, name)[0], keep); err != nil {
		t.Fatal(err)
	}
}

// ovfManager adds CreateDescriptor, which the simulator does not have.
type ovfManager struct {
	*simulator.OvfManager
	sim *Sim
}

func (m *ovfManager) CreateDescriptor(ctx *simulator.Context, req *types.CreateDescriptor) soap.HasFault {
	body := new(methods.CreateDescriptorBody)
	vm, ok := ctx.Map.Get(req.Obj).(*simulator.VirtualMachine)
	if !ok {
		body.Fault_ = simulator.Fault("not a virtual machine", &types.InvalidArgument{InvalidProperty: "obj"})
		return body
	}
	m.sim.mu.Lock()
	sizes := m.sim.sizes[vm.Name]
	m.sim.mu.Unlock()
	var refs, disks, items strings.Builder
	n := 0
	for _, f := range req.Cdp.OvfFiles {
		if !strings.HasSuffix(f.Path, ".vmdk") {
			continue
		}
		n++
		if n > len(sizes) {
			body.Fault_ = simulator.Fault("a disk the test did not plant", &types.InvalidArgument{InvalidProperty: "cdp"})
			return body
		}
		fmt.Fprintf(&refs, `    <File ovf:href="%s" ovf:id="file%d" ovf:size="%d"/>`+"\n", f.Path, n, f.Size)
		fmt.Fprintf(&disks, `    <Disk ovf:capacity="%d" ovf:capacityAllocationUnits="byte * 2^20" ovf:diskId="vmdisk%d" ovf:fileRef="file%d" ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>`+"\n", sizes[n-1]>>20, n, n)
		fmt.Fprintf(&items, `      <Item>
        <rasd:AddressOnParent>%d</rasd:AddressOnParent>
        <rasd:ElementName>Hard disk %d</rasd:ElementName>
        <rasd:HostResource>ovf:/disk/vmdisk%d</rasd:HostResource>
        <rasd:InstanceID>%d</rasd:InstanceID>
        <rasd:Parent>3</rasd:Parent>
        <rasd:ResourceType>17</rasd:ResourceType>
      </Item>
`, n-1, n, n, 10+n)
	}
	body.Res = &types.CreateDescriptorResponse{Returnval: types.OvfCreateDescriptorResult{OvfDescriptor: fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1" xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData" xmlns:vmw="http://www.vmware.com/schema/ovf">
  <References>
%s  </References>
  <DiskSection>
    <Info>Virtual disk information</Info>
%s  </DiskSection>
  <NetworkSection>
    <Info>The list of logical networks</Info>
    <Network ovf:name="VM Network"><Description>The VM Network network</Description></Network>
  </NetworkSection>
  <VirtualSystem ovf:id="%s">
    <Info>A virtual machine</Info>
    <Name>%s</Name>
    <OperatingSystemSection ovf:id="101" vmw:osType="otherLinux64Guest"><Info>The kind of installed guest operating system</Info></OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware requirements</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>%s</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>vmx-13</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>
        <rasd:ElementName>2 virtual CPU(s)</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>2</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
        <rasd:ElementName>64MB of memory</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>64</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:Address>0</rasd:Address>
        <rasd:ElementName>SCSI Controller 0</rasd:ElementName>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceSubType>lsilogic</rasd:ResourceSubType>
        <rasd:ResourceType>6</rasd:ResourceType>
      </Item>
%s      <Item>
        <rasd:AddressOnParent>7</rasd:AddressOnParent>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:Connection>VM Network</rasd:Connection>
        <rasd:ElementName>Network adapter 1</rasd:ElementName>
        <rasd:InstanceID>9</rasd:InstanceID>
        <rasd:ResourceSubType>VmxNet3</rasd:ResourceSubType>
        <rasd:ResourceType>10</rasd:ResourceType>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>
`, refs.String(), disks.String(), vm.Name, vm.Name, vm.Name, items.String())}}
	return body
}
