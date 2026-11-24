package hyperv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ajthom90/better-backup/internal/vhd"
)

// Checkpoint is a frozen view of a virtual machine's disks. While it
// exists, the files in Config.Disks do not change and can be read.
type Checkpoint struct {
	Config   Config
	Warnings []string
	id       string
	// recovery says the checkpoint is a recovery snapshot, which becomes
	// a reference point when it is released.
	recovery bool
	stateDir string
}

// tagPrefix starts the name of every checkpoint this program makes.
const tagPrefix = "better-backup "

// run executes a PowerShell script. Values reach the script through
// environment variables, never as script text.
func run(script string, env map[string]string) ([]byte, error) {
	// The script runs from a file: PowerShell reading a script from its
	// standard input carries on after an error and still exits with 0.
	f, err := os.CreateTemp("", "bb-*.ps1")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString("$ErrorActionPreference = 'Stop'\r\n$ProgressPreference = 'SilentlyContinue'\r\n" + script + "\r\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", f.Name())
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if i := strings.Index(msg, "\n"); i > 0 {
			msg = strings.TrimSpace(msg[:i])
		}
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "is not recognized as the name of a cmdlet") {
			return nil, fmt.Errorf("%w (%s)", ErrUnsupported, msg)
		}
		return nil, errors.New(msg)
	}
	return bytes.TrimSpace(out.Bytes()), nil
}

const listScript = `@(Get-VM | ForEach-Object { $_.Name }) | ConvertTo-Json -Compress`

// List returns the names of the virtual machines on this host.
func List() ([]string, error) {
	out, err := run(listScript, nil)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(out, &names); err != nil {
		var one string // PowerShell 5 prints a single item without brackets
		if json.Unmarshal(out, &one) != nil {
			return nil, fmt.Errorf("reading the list of virtual machines: %w", err)
		}
		names = []string{one}
	}
	return names, nil
}

// configScript prints a machine's configuration. With BB_CP set it
// lists the disks as of that checkpoint instead of the current ones.
const configScript = `
$vm = Get-VM -Name $env:BB_VM
$fw = $null
if ($vm.Generation -eq 2) { $fw = Get-VMFirmware -VM $vm }
$drives = Get-VMHardDiskDrive -VM $vm
if ($env:BB_CP) {
  $cp = @(Get-VMSnapshot -VM $vm) + @(Get-VMSnapshot -VM $vm -SnapshotType Recovery) | Where-Object { $_.Id.Guid -eq $env:BB_CP } | Select-Object -First 1
  if (-not $cp) { throw "the backup checkpoint of '$($vm.Name)' has disappeared" }
  $drives = Get-VMHardDiskDrive -VMSnapshot $cp
}
[pscustomobject]@{
  id = $vm.Id.Guid
  name = $vm.Name
  generation = [int]$vm.Generation
  version = "$($vm.Version)"
  state = "$($vm.State)"
  processors = [int]$vm.ProcessorCount
  memory_startup = [long]$vm.MemoryStartup
  dynamic_memory = [bool]$vm.DynamicMemoryEnabled
  memory_min = [long]$vm.MemoryMinimum
  memory_max = [long]$vm.MemoryMaximum
  secure_boot = [bool]($fw -and "$($fw.SecureBoot)" -eq 'On')
  secure_boot_template = $(if ($fw) { "$($fw.SecureBootTemplate)" } else { '' })
  checkpoint_type = "$($vm.CheckpointType)"
  notes = "$($vm.Notes)"
  disks = @($drives | ForEach-Object { [pscustomobject]@{
    controller_type = "$($_.ControllerType)"
    controller_number = [int]$_.ControllerNumber
    controller_location = [int]$_.ControllerLocation
    path = "$($_.Path)" } })
  nics = @(Get-VMNetworkAdapter -VM $vm | ForEach-Object { [pscustomobject]@{
    name = "$($_.Name)"
    switch = "$($_.SwitchName)"
    mac = "$($_.MacAddress)"
    dynamic_mac = [bool]$_.DynamicMacAddressEnabled } })
} | ConvertTo-Json -Depth 5 -Compress
`

// checkpointScript clears checkpoints left by an interrupted backup,
// makes a new one and prints its id.
const checkpointScript = `
$vm = Get-VM -Name $env:BB_VM
if ("$($vm.CheckpointType)" -eq 'Disabled') { throw "checkpoints are disabled for the virtual machine '$($vm.Name)'; allow them in its settings so it can be backed up" }
Get-VMSnapshot -VM $vm | Where-Object { $_.Name -like ($env:BB_PREFIX + '*') } | Remove-VMSnapshot -Confirm:$false
$cp = Checkpoint-VM -VM $vm -SnapshotName $env:BB_TAG -Passthru
$cp.Id.Guid
`

const releaseScript = `
$vm = Get-VM -Name $env:BB_VM
@(Get-VMSnapshot -VM $vm) + @(Get-VMSnapshot -VM $vm -SnapshotType Recovery) | Where-Object { $_.Id.Guid -eq $env:BB_CP } | Select-Object -First 1 | Remove-VMSnapshot -Confirm:$false
`

// wmiPrelude holds what the scripts that talk to Hyper-V's management
// interface share. Recovery snapshots and reference points have no
// PowerShell commands of their own.
const wmiPrelude = `
$ns = 'root\virtualization\v2'
$vm = Get-VM -Name $env:BB_VM
$svc = Get-WmiObject -Namespace $ns -Class Msvm_VirtualSystemSnapshotService
function Invoke-Hv($obj, $method, $params, $what) {
  $in = $obj.GetMethodParameters($method)
  foreach ($k in $params.Keys) { $in[$k] = $params[$k] }
  $r = $obj.InvokeMethod($method, $in, $null)
  if ($r.ReturnValue -eq 0) { return }
  if ($r.ReturnValue -ne 4096) { throw "$what failed with code $($r.ReturnValue)" }
  $job = [wmi]$r.Job
  while ($job.JobState -eq 2 -or $job.JobState -eq 3 -or $job.JobState -eq 4) { Start-Sleep -Milliseconds 500; $job.Get() }
  if ($job.JobState -ne 7) { throw "$what failed: $($job.ErrorDescription)" }
}
function Get-RefPoints { @(Get-WmiObject -Namespace $ns -Class Msvm_VirtualSystemReferencePoint | Where-Object { $_.VirtualSystemIdentifier -eq $vm.Id.Guid }) }
`

// recoveryScript removes the recovery snapshot an interrupted backup
// left (BB_OLD), makes a new one and prints its id. It first asks the
// guest to flush its applications and settles for a crash-consistent
// snapshot if the guest cannot.
const recoveryScript = wmiPrelude + `
if ($env:BB_OLD) { Get-VMSnapshot -VM $vm -SnapshotType Recovery | Where-Object { $_.Id.Guid -eq $env:BB_OLD } | Remove-VMSnapshot -Confirm:$false }
$cs = Get-WmiObject -Namespace $ns -Class Msvm_ComputerSystem -Filter "Name='$($vm.Id.Guid)'"
$before = @(Get-VMSnapshot -VM $vm -SnapshotType Recovery | ForEach-Object { $_.Id.Guid })
$done = $false
foreach ($level in 1, 2) {
  $sd = ([wmiclass]"\\.\${ns}:Msvm_VirtualSystemSnapshotSettingData").CreateInstance()
  $sd.ConsistencyLevel = $level
  $sd.IgnoreNonSnapshottableDisks = $true
  try {
    Invoke-Hv $svc 'CreateSnapshot' @{ AffectedSystem = $cs.__PATH; SnapshotSettings = $sd.GetText(2); SnapshotType = 32768 } 'creating a recovery snapshot'
    $done = $true
    break
  } catch { if ($level -eq 2) { throw } }
}
$new = @(Get-VMSnapshot -VM $vm -SnapshotType Recovery | Where-Object { $before -notcontains $_.Id.Guid })
if ($new.Count -ne 1) { throw "expected one new recovery snapshot, found $($new.Count)" }
$new[0].Id.Guid
`

// referenceScript turns the recovery snapshot into a reference point,
// which merges the snapshot away and keeps the change tracking going.
// It prints the new reference point's id and then removes the one this
// program made before (BB_OLDREF).
const referenceScript = wmiPrelude + `
$before = @(Get-RefPoints | ForEach-Object { $_.InstanceID })
$snap = Get-WmiObject -Namespace $ns -Class Msvm_VirtualSystemSettingData | Where-Object { $_.InstanceID -eq ('Microsoft:' + $env:BB_CP) }
if (-not $snap) { throw "the recovery snapshot has disappeared" }
Invoke-Hv $svc 'ConvertToReferencePoint' @{ AffectedSnapshot = $snap.__PATH } 'converting the snapshot to a reference point'
$new = @(Get-RefPoints | Where-Object { $before -notcontains $_.InstanceID })
if ($new.Count -ge 1) { $new[0].InstanceID }
if ($env:BB_OLDREF) {
  $old = Get-RefPoints | Where-Object { $_.InstanceID -eq $env:BB_OLDREF }
  if ($old) {
    $rsvc = Get-WmiObject -Namespace $ns -Class Msvm_VirtualSystemReferencePointService
    try { Invoke-Hv $rsvc 'DestroyReferencePoint' @{ AffectedReferencePoint = $old.__PATH } 'removing the old reference point' } catch { }
  }
}
`

// state is what is remembered about a machine between backups.
type state struct {
	Snapshot  string `json:"snapshot,omitempty"`  // a recovery snapshot being read
	Reference string `json:"reference,omitempty"` // the newest reference point
}

func stateFile(dir string) string { return filepath.Join(dir, "hyperv-state.json") }

func loadState(dir string) map[string]state {
	m := map[string]state{}
	if b, err := os.ReadFile(stateFile(dir)); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func saveState(dir, vm string, st state) {
	if dir == "" {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	m := loadState(dir)
	m[strings.ToLower(vm)] = st
	if b, err := json.Marshal(m); err == nil {
		os.WriteFile(stateFile(dir), b, 0o600)
	}
}

var stateMu sync.Mutex

// track fills in each disk's tracking id and, where the previous backup
// left an id the disk still knows, what was written since.
func (c *Checkpoint) track(prev *Config) {
	for i := range c.Config.Disks {
		d := &c.Config.Disks[i]
		id, err := vhd.Tracking(d.Path)
		if err != nil || id == "" {
			continue
		}
		d.TrackingID = id
		if prev == nil {
			continue
		}
		for _, p := range prev.Disks {
			if p.File != d.File || p.TrackingID == "" {
				continue
			}
			if ch, err := vhd.Changes(d.Path, p.TrackingID); err == nil {
				d.Tracked = true
				for _, r := range ch {
					d.Changed = append(d.Changed, Range{r.Off, r.Len})
				}
			}
		}
	}
}

// Prepare records a machine's configuration and takes a checkpoint of
// it. The returned Config lists the disk files as they were at the
// checkpoint; they stay unchanged until Release. tag names the
// checkpoint in Hyper-V Manager.
//
// With o.Tracking the checkpoint is a recovery snapshot, the kind
// backup programs use: it leaves change tracking behind when released.
// If Hyper-V will not make one, an ordinary checkpoint is used and the
// disks are read in full.
func Prepare(name, tag string, o Options) (*Checkpoint, error) {
	env := map[string]string{"BB_VM": name, "BB_TAG": tagPrefix + tag, "BB_PREFIX": tagPrefix}
	c := &Checkpoint{stateDir: o.StateDir}
	if o.Tracking {
		st := loadState(o.StateDir)[strings.ToLower(name)]
		env["BB_OLD"] = st.Snapshot
		if out, err := run(recoveryScript, env); err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: change tracking could not be used, the disks are read in full: %v", name, err))
		} else {
			c.id, c.recovery = strings.TrimSpace(string(out)), true
			st.Snapshot = c.id
			saveState(o.StateDir, name, st)
		}
	}
	if c.id == "" {
		out, err := run(checkpointScript, env)
		if err != nil {
			return nil, fmt.Errorf("checkpoint of %s: %w", name, err)
		}
		c.id = strings.TrimSpace(string(out))
	}
	env["BB_CP"] = c.id
	out, err := run(configScript, env)
	if err == nil {
		err = json.Unmarshal(out, &c.Config)
	}
	if err != nil {
		c.Config.Name = name
		c.Release()
		return nil, fmt.Errorf("reading the configuration of %s: %w", name, err)
	}
	var disks []Disk
	for _, d := range c.Config.Disks {
		if !isDiskFile(d.Path) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: the drive at %s %d:%d (%s) is not a virtual disk file and was not backed up",
				name, d.ControllerType, d.ControllerNumber, d.ControllerLocation, d.Path))
			continue
		}
		d.File = d.FileName()
		disks = append(disks, d)
	}
	c.Config.Disks = disks
	if c.recovery {
		c.track(o.Previous)
	}
	return c, nil
}

// Release removes the checkpoint. Hyper-V then merges the changes made
// since into the disks, in the background.
func (c *Checkpoint) Release() error {
	if c == nil || c.id == "" {
		return nil
	}
	env := map[string]string{"BB_VM": c.Config.Name, "BB_CP": c.id}
	c.id = ""
	if !c.recovery {
		_, err := run(releaseScript, env)
		return err
	}
	st := loadState(c.stateDir)[strings.ToLower(c.Config.Name)]
	env["BB_OLDREF"] = st.Reference
	out, err := run(referenceScript, env)
	if ref, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); err == nil || ref != "" {
		// The snapshot is gone and a new reference point stands.
		if ref = strings.TrimSpace(ref); ref != "" {
			st.Reference = ref
		}
		st.Snapshot = ""
		saveState(c.stateDir, c.Config.Name, st)
		return err
	}
	// No reference point: remove the snapshot the ordinary way. The
	// next backup then reads the disks in full.
	if _, rerr := run(releaseScript, env); rerr != nil {
		return rerr
	}
	st.Snapshot = ""
	saveState(c.stateDir, c.Config.Name, st)
	return nil
}

// createScript builds a machine from a configuration file and the disk
// files beside it. Lines it prints are warnings.
const createScript = `
$c = Get-Content -Raw -LiteralPath $env:BB_CFG | ConvertFrom-Json
if (Get-VM -Name $env:BB_NAME -ErrorAction SilentlyContinue) { throw "a virtual machine named '$($env:BB_NAME)' already exists; choose another name" }
$vm = New-VM -Name $env:BB_NAME -Generation $c.generation -MemoryStartupBytes $c.memory_startup -NoVHD -Path $env:BB_DIR
try {
  Set-VMProcessor -VM $vm -Count $c.processors
  if ($c.dynamic_memory) {
    Set-VMMemory -VM $vm -DynamicMemoryEnabled $true -MinimumBytes $c.memory_min -MaximumBytes $c.memory_max -StartupBytes $c.memory_startup
  } else {
    Set-VMMemory -VM $vm -DynamicMemoryEnabled $false
  }
  if ($c.notes) { Set-VM -VM $vm -Notes $c.notes }
  Get-VMDvdDrive -VM $vm | Remove-VMDvdDrive
  Get-VMNetworkAdapter -VM $vm | Remove-VMNetworkAdapter
  foreach ($n in $c.nics) {
    $a = @{ VM = $vm; Name = $n.name }
    if ($n.switch -and (Get-VMSwitch -Name $n.switch -ErrorAction SilentlyContinue)) { $a.SwitchName = $n.switch }
    elseif ($n.switch) { "the virtual switch '$($n.switch)' does not exist on this host; the adapter '$($n.name)' is not connected" }
    if (-not $n.dynamic_mac -and $n.mac) { $a.StaticMacAddress = $n.mac }
    Add-VMNetworkAdapter @a
  }
  $first = $null
  foreach ($d in $c.disks) {
    if ($d.controller_type -eq 'SCSI') {
      while (@(Get-VMScsiController -VM $vm).Count -le $d.controller_number) { Add-VMScsiController -VM $vm }
    }
    $drive = Add-VMHardDiskDrive -VM $vm -ControllerType $d.controller_type -ControllerNumber $d.controller_number -ControllerLocation $d.controller_location -Path (Join-Path $env:BB_DIR $d.file) -Passthru
    if (-not $first) { $first = $drive }
  }
  if ($c.generation -eq 2) {
    if ($c.secure_boot) {
      if ($c.secure_boot_template) { Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate $c.secure_boot_template }
      else { Set-VMFirmware -VM $vm -EnableSecureBoot On }
    } else { Set-VMFirmware -VM $vm -EnableSecureBoot Off }
    if ($first) { Set-VMFirmware -VM $vm -FirstBootDevice $first }
  }
} catch {
  Remove-VM -VM $vm -Force
  throw
}
`

// Create builds a new virtual machine named name from cfg. The disk
// images must already be in dir under the names in cfg.Disks[].File;
// the machine's own files go there too. The machine is left switched
// off. It returns warnings about things that could not be put back.
func Create(cfg Config, dir, name string) ([]string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".bb-vm-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	out, err := run(createScript, map[string]string{"BB_CFG": f.Name(), "BB_NAME": name, "BB_DIR": dir})
	if err != nil {
		return nil, err
	}
	var warnings []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			warnings = append(warnings, line)
		}
	}
	return warnings, nil
}
