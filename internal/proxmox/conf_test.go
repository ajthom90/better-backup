package proxmox

import (
	"reflect"
	"testing"
)

func TestConf(t *testing.T) {
	conf := parseConf("# a comment\nname: web01\nagent: enabled=1,fstrim_cloned_disks=1\n" +
		"scsi0: file=local-lvm:vm-101-disk-0,size=32G,ssd=1\nsata0: local:iso/debian.iso,media=cdrom\n" +
		"ide2: local-lvm:vm-101-cloudinit,media=cdrom\nvirtio0: local:101/vm-101-disk-1.vmdk\ncipassword: **********\n" +
		"description: a%3Ab\n\n[snap]\nname: old\n")
	if confValue(conf, "name") != "web01" || !agentEnabled(conf) || agentEnabled(parseConf("agent: 0")) || agentEnabled(nil) {
		t.Fatalf("conf: %+v", conf)
	}
	ds, warnings := disks(conf)
	want := []Disk{
		{Key: "scsi0", File: "disk-scsi0", Volume: "local-lvm:vm-101-disk-0", Storage: "local-lvm", Options: "size=32G,ssd=1", Format: "raw"},
		{Key: "virtio0", File: "disk-virtio0", Volume: "local:101/vm-101-disk-1.vmdk", Storage: "local", Format: "vmdk"},
	}
	if !reflect.DeepEqual(ds, want) || len(warnings) != 0 {
		t.Fatalf("disks: %+v, warnings %v", ds, warnings)
	}
	first, rest, warnings := restoreSettings(conf, map[string]string{"scsi0": "tank:vm-105-disk-0", "virtio0": "tank:vm-105-disk-1"}, "tank")
	wantFirst := []setting{{"scsi0", "tank:vm-105-disk-0,size=32G,ssd=1"}, {"sata0", "local:iso/debian.iso,media=cdrom"},
		{"ide2", "tank:cloudinit,media=cdrom"}, {"virtio0", "tank:vm-105-disk-1"}}
	wantRest := []setting{{"agent", "enabled=1,fstrim_cloned_disks=1"}, {"description", "a:b"}, {"vmgenid", "1"}}
	if !reflect.DeepEqual(first, wantFirst) || !reflect.DeepEqual(rest, wantRest) || len(warnings) != 1 {
		t.Fatalf("first %+v\nrest %+v\nwarnings %v", first, rest, warnings)
	}
	// Without a target storage a cloud-init drive stays where it was.
	first, _, _ = restoreSettings(conf, nil, "")
	if first[1] != (setting{"ide2", "local-lvm:cloudinit,media=cdrom"}) {
		t.Fatalf("cloud-init drive: %+v", first)
	}
	if got := bitmapPrefix(""); got != "bb-manual-" {
		t.Fatal(got)
	}
}
