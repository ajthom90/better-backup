package restore

import "testing"

func TestClassify(t *testing.T) {
	const cfg = `C:\Windows\System32\config`
	for _, c := range []struct {
		dest string
		kind int
		hive string
	}{
		{`C:\Windows\System32\config\SYSTEM`, fileHive, "SYSTEM"},
		{`c:\windows\system32\CONFIG\software`, fileHive, "SOFTWARE"},
		{`C:\Windows\System32\config\DEFAULT`, fileHive, "DEFAULT"},
		{`C:\Windows\System32\config\SYSTEM.LOG1`, fileHiveLog, "SYSTEM.LOG1"},
		{`C:\Windows\System32\config\software.log`, fileHiveLog, "SOFTWARE.LOG"},
		{`C:\Windows\System32\config\SOFTWARE{53b39e88-18c4-11ea-a811-000d3aa4692b}.TM.blf`, fileSkip, ""},
		{`C:\Windows\System32\config\COMPONENTS`, fileOrdinary, ""},
		{`C:\Windows\System32\config\systemprofile\SYSTEM`, fileOrdinary, ""},
		{`C:\Windows\System32\config\RegBack\SYSTEM`, fileOrdinary, ""},
		{`D:\data\SYSTEM`, fileOrdinary, ""},
	} {
		if kind, hive := classify(cfg, c.dest); kind != c.kind || hive != c.hive {
			t.Errorf("classify(%s) = %d %q, want %d %q", c.dest, kind, hive, c.kind, c.hive)
		}
	}
}

func TestParseKeep(t *testing.T) {
	for _, c := range []struct {
		entry      string
		kind       int
		key, value string
		ok         bool
	}{
		{`MountedDevices\`, keepTree, `MountedDevices`, "", true},
		{`CurrentControlSet\Services\*`, keepMerge, `CurrentControlSet\Services`, "", true},
		{`CurrentControlSet\Control\Session Manager\PendingFileRenameOperations`, keepValue, `CurrentControlSet\Control\Session Manager`, "PendingFileRenameOperations", true},
		{` \CurrentControlSet\Enum\ `, keepTree, `CurrentControlSet\Enum`, "", true},
		{``, 0, "", "", false},
		{`*`, 0, "", "", false},
		{`novalue`, 0, "", "", false},
	} {
		kind, key, value, ok := parseKeep(c.entry)
		if ok != c.ok || ok && (kind != c.kind || key != c.key || value != c.value) {
			t.Errorf("parseKeep(%q) = %d %q %q %v", c.entry, kind, key, value, ok)
		}
	}
	for in, want := range map[string]string{
		`CurrentControlSet\Services\X`: `ControlSet002\Services\X`,
		`currentcontrolset`:            `ControlSet002`,
		`CurrentControlSetX\Y`:         `CurrentControlSetX\Y`,
		`MountedDevices`:               `MountedDevices`,
	} {
		if got := controlSet(in, "ControlSet002"); got != want {
			t.Errorf("controlSet(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestADLog(t *testing.T) {
	for name, want := range map[string]bool{"edb.log": true, "EDB00012.log": true, "edb.chk": true, "edbres00001.jrs": true, "temp.edb": true,
		"ntds.dit": false, "ntds.jfm": false, "notes.log": false} {
		if adLog(name) != want {
			t.Errorf("adLog(%q) = %v", name, !want)
		}
	}
}
