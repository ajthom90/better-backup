package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

const sample = `<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>web &amp; db</name>
  <uuid>6f1c0a52-1c7e-4a5c-9d7e-0d6f6a1b2c3d</uuid>
  <!-- <name>not this one</name> -->
  <metadata><app:info xmlns:app='urn:x'><name>nor this</name></app:info></metadata>
  <os>
    <type arch='x86_64' machine='pc-q35-8.2'>hvm</type>
    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd' format='raw'>/var/lib/libvirt/qemu/nvram/web_VARS.fd</nvram>
  </os>
  <devices>
    <disk type='file' device='disk' snapshot='external'>
      <driver name='qemu' type='raw' cache='none' io="native"/>
      <source file='/var/lib/libvirt/images/web.img' index='2'>
        <encryption format='luks'><secret type='passphrase' uuid='x'/></encryption>
      </source>
      <backingStore type='file'><format type='raw'/><source file='/base.img'/><backingStore/></backingStore>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='block' device='disk'>
      <source dev='/dev/vg0/web-data'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='network' device='disk'>
      <driver name='qemu' type='raw'/>
      <source protocol='rbd' name='pool/image'><host name='ceph' port='6789'/></source>
      <target dev='vdc' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/iso/install.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='block' device='lun'><source dev='/dev/sdq'/><target dev='sdb' bus='scsi'/></disk>
    <disk type='file' device='disk'><source file='/shared.img'/><target dev='vdd' bus='virtio'/><shareable/></disk>
    <tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>
  </devices>
  <qemu:commandline><qemu:arg value='-snapshot'/></qemu:commandline>
</domain>
`

func TestDomain(t *testing.T) {
	d, err := parseDomain([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	disks, warnings := d.disks()
	if d.Name != "web & db" || !d.swtpm() || strings.TrimSpace(d.OS.NVRAM.Path) != "/var/lib/libvirt/qemu/nvram/web_VARS.fd" {
		t.Fatalf("parsed: %+v", d)
	}
	if len(disks) != 2 || disks[0] != (Disk{Target: "vda", File: "disk-vda", Path: "/var/lib/libvirt/images/web.img", Format: "raw"}) ||
		disks[1] != (Disk{Target: "vdb", File: "disk-vdb", Path: "/dev/vg0/web-data", Format: "raw", Block: true}) {
		t.Fatalf("disks: %+v", disks)
	}
	if len(warnings) != 3 || !strings.Contains(warnings[0], "vdc") || !strings.Contains(warnings[1], "sdb") || !strings.Contains(warnings[2], "vdd") {
		t.Fatalf("warnings: %q", warnings)
	}
}

func TestRewrite(t *testing.T) {
	out, err := rewrite([]byte(sample), `copy <1> & 'two'`, false, map[string]string{"vda": "/restore/it's/vda.qcow2", "vdb": "/restore/vdb.qcow2"}, "/restore/VARS.fd")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if err := xml.Unmarshal(out, new(struct{})); err != nil {
		t.Fatalf("the result is not well formed: %v\n%s", err, got)
	}
	d, err := parseDomain(out)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != `copy <1> & 'two'` || d.UUID != "" || d.OS.NVRAM.Path != "/restore/VARS.fd" {
		t.Fatalf("name %q, uuid %q, nvram %q", d.Name, d.UUID, d.OS.NVRAM.Path)
	}
	disks, _ := d.disks()
	if len(disks) != 2 || disks[0].Path != "/restore/it's/vda.qcow2" || disks[0].Format != "qcow2" || disks[0].Block ||
		disks[1].Path != "/restore/vdb.qcow2" || disks[1].Format != "qcow2" || disks[1].Block {
		t.Fatalf("disks: %+v", disks)
	}
	for _, want := range []string{
		"<!-- <name>not this one</name> -->", "<name>nor this</name>", "xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'",
		"<qemu:commandline><qemu:arg value='-snapshot'/></qemu:commandline>", "<disk type='file' device='disk'>\n      <driver name='qemu' cache='none' io='native' type='qcow2'/>",
		"<nvram format='raw'>/restore/VARS.fd</nvram>", "<source protocol='rbd' name='pool/image'><host name='ceph' port='6789'/></source>",
		"<source file='/iso/install.iso'/>", "<disk type='block' device='lun'><source dev='/dev/sdq'/>",
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/>\n      <source file='/restore/vdb.qcow2'/>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the result lacks %s", want)
		}
	}
	for _, gone := range []string{"web.img", "backingStore", "encryption", "/dev/vg0", "snapshot='external'", "<uuid>", "web_VARS", "template="} {
		if strings.Contains(got, gone) {
			t.Errorf("the result still has %s", gone)
		}
	}
	if t.Failed() {
		t.Log(got)
	}

	// Nothing to change but the name: everything else is byte for byte.
	out, err = rewrite([]byte(sample), "web & db", true, nil, "")
	if err != nil || string(out) != sample {
		t.Fatalf("an unchanged rewrite differs: %v\n%s", err, out)
	}
	if _, err := rewrite([]byte("<domain><devices>"), "x", true, nil, ""); err == nil {
		t.Fatal("a broken definition was accepted")
	}
	if _, err := rewrite([]byte("<domain><uuid>1</uuid></domain>"), "x", true, nil, ""); err == nil {
		t.Fatal("a definition without a name was accepted")
	}
}

func TestBackupXML(t *testing.T) {
	cfg := &Config{Name: "web", Disks: []Disk{{Target: "vda", File: "disk-vda", Bitmap: true}, {Target: "vdb", File: "disk-vdb", Block: true}}}
	prev := &Config{Disks: []Disk{{Target: "vda", File: "disk-vda", Bitmap: true}, {Target: "vdb", File: "disk-vdb"}}}
	got := backupXML(cfg, prev, "/run/a'b.sock", "bb-1", "/scratch")
	want := "<domainbackup mode='pull'><incremental>bb-1</incremental><server transport='unix' socket='/run/a&#39;b.sock'/><disks>" +
		"<disk name='vda' backup='yes' type='file'></disk>" +
		"<disk name='vdb' backup='yes' type='file' backupmode='full'><scratch file='/scratch/bb-scratch-web-vdb.qcow2'/></disk></disks></domainbackup>"
	if got != want {
		t.Fatalf("backup XML:\n%s\nwant:\n%s", got, want)
	}
	if got := checkpointXML(cfg, "bb-2"); !strings.Contains(got, "<disk name='vda' checkpoint='bitmap'/><disk name='vdb' checkpoint='no'/>") {
		t.Fatalf("checkpoint XML: %s", got)
	}
	if got := checkpointPrefix(""); got != "bb-manual-" {
		t.Fatal(got)
	}
	table := ` Name                Creation Time
----------------------------------------------
 bb-aaaa-1           2026-11-24 00:45:12 +0000
 bb-bbbb-1           2026-09-01 10:00:00 +0200
 bb-bbbb-2           2026-11-01 10:00:00 +0200
 nightly snapshot    2020-01-01 00:00:00 +0000
`
	have, stale := checkpoints(table, "bb-aaaa-", time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	if len(have) != 4 || !have["bb-aaaa-1"] || !have["nightly snapshot"] || len(stale) != 1 || stale[0] != "bb-bbbb-1" {
		t.Fatalf("checkpoints: %v, stale %v", have, stale)
	}
}
