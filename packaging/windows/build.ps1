<#
  Builds the Windows installers:
    better-backup-agent_VERSION_windows_ARCH.msi
    better-backup-server_VERSION_windows_ARCH.msi

  Needs Go and the .NET SDK; it installs the WiX toolset itself.

    pwsh packaging/windows/build.ps1 -Version 1.2.0 [-Arch amd64|arm64] [-OutDir dist]

  To sign the programs and installers, set SIGN_PFX_BASE64 (a code
  signing certificate as base64 PFX) and SIGN_PFX_PASSWORD. Without them
  the installers are built unsigned.
#>
param(
  [Parameter(Mandatory)] [string] $Version,
  [ValidateSet('amd64', 'arm64')] [string] $Arch = 'amd64',
  [string] $OutDir = 'dist'
)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true
$root = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$wixVersion = '5.0.2'

# Windows Installer versions are three numbers; 1.2.0-rc1 installs as 1.2.0.
$Version = $Version -replace '^v', ''
if ($Version -notmatch '^(\d+)\.(\d+)\.(\d+)') { throw "version must start with three numbers such as 1.2.0, got $Version" }
$msiVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3])"

$out = New-Item -ItemType Directory -Force (Join-Path $root $OutDir)
$bin = New-Item -ItemType Directory -Force (Join-Path $out "bin-$Arch")

Push-Location $root
try {
  $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = $Arch
  $ld = "-s -w -X github.com/ajthom90/better-backup/internal/server.Version=$Version -X github.com/ajthom90/better-backup/internal/agent.Version=$Version"
  go build -trimpath -ldflags $ld -o "$bin/" ./cmd/...
} finally {
  Remove-Item Env:GOOS, Env:GOARCH
  Pop-Location
}

function Sign([string[]] $files) {
  if (-not $env:SIGN_PFX_BASE64) { return }
  $pfx = Join-Path ([IO.Path]::GetTempPath()) "bb-sign-$PID.pfx"
  [IO.File]::WriteAllBytes($pfx, [Convert]::FromBase64String($env:SIGN_PFX_BASE64))
  try {
    $signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" | Sort-Object FullName | Select-Object -Last 1
    if (-not $signtool) { throw 'signtool.exe was not found; install the Windows SDK' }
    & $signtool.FullName sign /f $pfx /p $env:SIGN_PFX_PASSWORD /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 @files
  } finally { Remove-Item $pfx -Force }
}
Sign (Get-ChildItem "$bin\*.exe").FullName

if (-not (Get-Command wix -ErrorAction SilentlyContinue)) {
  dotnet tool install --global wix --version $wixVersion
  $env:PATH += ";$env:USERPROFILE\.dotnet\tools"
}
$exts = 'WixToolset.UI.wixext', 'WixToolset.Util.wixext', 'WixToolset.Firewall.wixext'
foreach ($e in $exts) { wix extension add --global "$e/$wixVersion" }
$extArgs = $exts | ForEach-Object { '-ext'; $_ }
$wixArch = @{ amd64 = 'x64'; arm64 = 'arm64' }[$Arch]

$agentName = "better-backup-agent_${Version}_windows_$Arch.msi"
$serverName = "better-backup-server_${Version}_windows_$Arch.msi"
$agentMsi = Join-Path $out $agentName
$serverMsi = Join-Path $out $serverName

wix build -arch $wixArch -culture en-US @extArgs -d "Version=$msiVersion" -d "BinDir=$bin" `
  -o $agentMsi (Join-Path $PSScriptRoot 'agent.wxs') (Join-Path $PSScriptRoot 'ui.wxs')
Sign $agentMsi

# The server carries the agent installer, signed, to hand out from its web UI.
wix build -arch $wixArch -culture en-US @extArgs -d "Version=$msiVersion" -d "BinDir=$bin" `
  -d "AgentMsi=$agentMsi" -d "AgentMsiName=$agentName" `
  -o $serverMsi (Join-Path $PSScriptRoot 'server.wxs') (Join-Path $PSScriptRoot 'ui.wxs')
Sign $serverMsi

Get-ChildItem $out -Filter *.wixpdb | Remove-Item
Write-Host "built $agentMsi"
Write-Host "built $serverMsi"
