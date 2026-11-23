package vss

import (
	"reflect"
	"testing"
)

const registryDoc = `<WRITER_METADATA xmlns="x-schema:#VssWriterMetadataInfo" version="1.2" backupSchema="0">
<IDENTIFICATION writerId="afbab4a2-367d-4d15-a586-71dbb18f8485" instanceId="x" friendlyName="Registry Writer" usage="BOOTABLE_SYSTEM_STATE" dataSource="OTHER"/>
<RESTORE_METHOD method="CUSTOM" writerRestore="never" rebootRequired="yes"/>
<BACKUP_LOCATIONS>
 <FILE_GROUP componentName="Registry" caption="Registry" restoreMetadata="no" notifyOnBackupComplete="no" selectable="no">
  <FILE_LIST path="C:\Windows\system32\config" filespec="SYSTEM" filespecBackupType="3855"/>
  <FILE_LIST path="c:\windows\System32\Config\" filespec="software"/>
  <FILE_LIST path="C:\Users\Default" filespec="NTUSER.DAT" alternatePath="D:\Moved"/>
 </FILE_GROUP>
 <DATABASE componentName="ntds" logicalPath="C:_Windows_NTDS">
  <DATABASE_FILES path="C:\Windows\NTDS" filespec="ntds.dit"/>
  <DATABASE_LOGFILES path="C:\Windows\NTDS" filespec="edb*.log"/>
  <DATABASE_LOGFILES path="C:\Windows\SYSVOL\domain" filespec="*.*" recursive="yes"/>
 </DATABASE>
</BACKUP_LOCATIONS>
<EXCLUDE_FILES>
 <EXCLUDE path="C:\Windows\SYSVOL\domain\DfsrPrivate" filespec="*" recursive="yes"/>
 <EXCLUDE path="C:\Windows\NTDS" filespec="edbtmp.log"/>
</EXCLUDE_FILES>
</WRITER_METADATA>`

func TestParseWriter(t *testing.T) {
	w, err := parseWriter(registryDoc)
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "Registry Writer" || w.ID != "afbab4a2-367d-4d15-a586-71dbb18f8485" || !w.IsSystemState() {
		t.Fatalf("identity: %+v", w)
	}
	if len(w.Components) != 2 || w.Components[0].Name != "Registry" || len(w.Components[0].Files) != 3 || len(w.Components[1].Files) != 3 {
		t.Fatalf("components: %+v", w.Components)
	}
	if got := w.Components[0].Files[2]; got != (FileSpec{Path: `D:\Moved`, Spec: "NTUSER.DAT"}) {
		t.Fatalf("alternate path: %+v", got)
	}
	if got := w.Components[1].Files[2]; !got.Recursive {
		t.Fatalf("recursive: %+v", got)
	}
	if len(w.Excludes) != 2 {
		t.Fatalf("excludes: %+v", w.Excludes)
	}
}

func TestSystemStateWriters(t *testing.T) {
	sys := Writer{Name: "System Writer", Usage: "BOOTABLE_SYSTEM_STATE", Components: []Component{{Name: "a", Files: []FileSpec{{Path: `C:\Windows`, Spec: "x"}}}}}
	sql := Writer{Name: "SqlServerWriter", Usage: "USER_DATA", Components: []Component{{Name: "db", Files: []FileSpec{{Path: `D:\Data`, Spec: "*"}}}},
		Excludes: []FileSpec{{Path: `C:\Temp`, Spec: "*"}}}
	dfsr := Writer{Name: "DFS Replication service writer", Usage: "USER_DATA", Components: []Component{{Name: "sysvol", Files: []FileSpec{{Path: `C:\Windows\SYSVOL`, Spec: "*", Recursive: true}}}}}
	empty := Writer{Name: "ASR Writer", Usage: "BOOTABLE_SYSTEM_STATE"}
	files, excludes, names := SystemState([]Writer{sys, sql, dfsr, empty})
	if len(files) != 2 || len(excludes) != 1 {
		t.Fatalf("files %v excludes %v", files, excludes)
	}
	if !reflect.DeepEqual(names, []string{"DFS Replication service writer", "System Writer"}) {
		t.Fatalf("names %v", names)
	}
	if !(Writer{Name: "NTDS"}).IsActiveDirectory() || !(Writer{Name: "NTDS"}).IsSystemState() {
		t.Fatal("NTDS is the Active Directory writer")
	}
}

func TestSelection(t *testing.T) {
	w, _ := parseWriter(registryDoc)
	files, excludes, _ := SystemState([]Writer{w})
	files = append(files, FileSpec{Path: `\\?\Volume{1}\x`, Spec: "*"}, FileSpec{Path: `%unknown%\x`, Spec: "*"})
	s := NewSelection(files, excludes)
	if got := s.Roots(); !reflect.DeepEqual(got, []string{`C:\`, `D:\`}) {
		t.Fatalf("roots %v", got)
	}
	for _, c := range []struct {
		path string
		dir  bool
		want bool
	}{
		{`C:\`, true, true},
		{`C:\Windows`, true, true},
		{`C:\WINDOWS\System32`, true, true},
		{`C:\Windows\System32\config`, true, true},
		{`C:\Windows\System32\config\SYSTEM`, false, true},
		{`C:\Windows\System32\config\Software`, false, true},
		{`C:\Windows\System32\config\SAM`, false, false},
		{`C:\Windows\System32\config\systemprofile`, true, false},
		{`C:\Windows\System32\drivers`, true, false},
		{`C:\Windows\notepad.exe`, false, false},
		{`C:\Users`, true, false}, // the file list moved to D:\Moved
		{`D:\Moved\ntuser.dat`, false, true},
		{`C:\Windows\NTDS\ntds.dit`, false, true},
		{`C:\Windows\NTDS\edb00001.log`, false, true},
		{`C:\Windows\NTDS\edbtmp.log`, false, false},
		{`C:\Windows\NTDS\temp.edb`, false, false},
		{`C:\Windows\SYSVOL`, true, true},
		{`C:\Windows\SYSVOL\domain\Policies\{31B2}\GPT`, true, true},
		{`C:\Windows\SYSVOL\domain\Policies\{31B2}\GPT.INI`, false, true},
		{`C:\Windows\SYSVOL\domain\scripts\noext`, false, true},
		{`C:\Windows\SYSVOL\domain\DfsrPrivate\x\y.db`, false, false},
		{`C:\Windows\SYSVOL\staging`, true, false},
		{`E:\x`, false, false},
		{`relative`, false, false},
	} {
		if got := s.Match(c.path, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}
