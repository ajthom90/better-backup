package hyperv

import "testing"

func TestPlaceBeside(t *testing.T) {
	for disk, want := range map[string]string{
		`D:\Hyper-V\web01\Virtual Hard Disks\web01.vhdx`: `D:\Hyper-V\web01`,
		`D:\VMs\web01.vhdx`:          `D:\VMs`,
		`D:\web01.vhdx`:              `D:\`,
		`\\nas\vms\web01\web01.vhdx`: `\\nas\vms`,
		``:                           ``,
	} {
		if got := placeBeside(disk); got != want {
			t.Errorf("placeBeside(%q) = %q, want %q", disk, got, want)
		}
	}
}
