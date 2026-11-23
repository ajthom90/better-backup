package hyperv

import (
	"encoding/json"
	"testing"
)

func TestConfig(t *testing.T) {
	// The shape the configuration script prints.
	raw := `{"id":"1b0c","name":"web01","generation":2,"version":"10.0","state":"Running","processors":4,"memory_startup":4294967296,
		"dynamic_memory":true,"memory_min":536870912,"memory_max":8589934592,"secure_boot":true,"secure_boot_template":"MicrosoftWindows",
		"checkpoint_type":"Production","notes":"",
		"disks":[{"controller_type":"SCSI","controller_number":0,"controller_location":1,"path":"D:\\VMs\\web01\\data.vhdx"}],
		"nics":[{"name":"Network Adapter","switch":"LAN","mac":"00155D010203","dynamic_mac":false}]}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.MemoryStartup != 4<<30 || !c.SecureBoot || len(c.Disks) != 1 || c.NICs[0].Switch != "LAN" {
		t.Fatalf("parsed: %+v", c)
	}
	if got := c.Disks[0].FileName(); got != "disk-scsi-0-1.vhdx" {
		t.Fatalf("file name %q", got)
	}
	for path, want := range map[string]bool{`D:\a.vhdx`: true, `D:\a_1B2C.AVHDX`: true, `D:\old.vhd`: true, `Disk 2 10.00 GB Bus 0 Lun 1 Target 0`: false, "": false} {
		if isDiskFile(path) != want {
			t.Errorf("isDiskFile(%q) = %v", path, !want)
		}
	}
}
