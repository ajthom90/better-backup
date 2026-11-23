package hyperv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Checkpoint is a frozen view of a virtual machine's disks. While it
// exists, the files in Config.Disks do not change and can be read.
type Checkpoint struct {
	Config   Config
	Warnings []string
	id       string
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
  $cp = Get-VMSnapshot -VM $vm | Where-Object { $_.Id.Guid -eq $env:BB_CP }
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
Get-VMSnapshot -VM $vm | Where-Object { $_.Id.Guid -eq $env:BB_CP } | Remove-VMSnapshot -Confirm:$false
`

// Prepare records a machine's configuration and takes a checkpoint of
// it. The returned Config lists the disk files as they were at the
// checkpoint; they stay unchanged until Release. tag names the
// checkpoint in Hyper-V Manager.
func Prepare(name, tag string) (*Checkpoint, error) {
	env := map[string]string{"BB_VM": name, "BB_TAG": tagPrefix + tag, "BB_PREFIX": tagPrefix}
	out, err := run(checkpointScript, env)
	if err != nil {
		return nil, fmt.Errorf("checkpoint of %s: %w", name, err)
	}
	c := &Checkpoint{id: strings.TrimSpace(string(out))}
	env["BB_CP"] = c.id
	out, err = run(configScript, env)
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
	return c, nil
}

// Release removes the checkpoint. Hyper-V then merges the changes made
// since into the disks, in the background.
func (c *Checkpoint) Release() error {
	if c == nil || c.id == "" {
		return nil
	}
	_, err := run(releaseScript, map[string]string{"BB_VM": c.Config.Name, "BB_CP": c.id})
	c.id = ""
	return err
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
		return nil, fmt.Errorf("creating the virtual machine %s: %w", name, err)
	}
	var warnings []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			warnings = append(warnings, line)
		}
	}
	return warnings, nil
}
