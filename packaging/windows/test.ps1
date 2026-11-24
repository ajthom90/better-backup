<#
  Installs the Windows installers for real, as CI does: the server, then
  an agent that enrolls with it from an enrollment code, then an upgrade
  of both, then removal. Run it elevated on a machine you can throw away.

    pwsh packaging/windows/test.ps1 -Dir dist -Old 0.0.1 -New 0.0.2

  Dir holds the installers of two versions built by build.ps1.
#>
param([string] $Dir = 'dist', [string] $Old = '0.0.1', [string] $New = '0.0.2', [int] $Port = 18443)
$ErrorActionPreference = 'Stop'
$Dir = Resolve-Path $Dir
$repo = 'C:\bb-test-repo'
$data = "$env:ProgramData\better-backup"
$state = "$env:ProgramData\better-backup-agent"

function Check($ok, [string] $what) {
  if (-not $ok) { throw "FAILED: $what" }
  Write-Host "ok: $what"
}
function Msi([string[]] $msiArgs, [string] $what) {
  $log = Join-Path $Dir ("msi-" + ($what -replace '\W+', '-') + ".log")
  $p = Start-Process msiexec -ArgumentList ($msiArgs + '/qn', '/norestart', '/l*v', "`"$log`"") -Wait -PassThru
  if ($p.ExitCode -ne 0) {
    Get-Content $log -Tail 80 | Write-Host
    foreach ($f in "$data\server.log", "$state\agent.log") { if (Test-Path $f) { Write-Host "--- $f"; Get-Content $f -Tail 40 | Write-Host } }
    throw "FAILED: $what (msiexec exit code $($p.ExitCode))"
  }
  Write-Host "ok: $what"
}
function Api([string] $path, [string] $method = 'GET', $body = $null) {
  $token = (Get-Content "$data\admin.json" | ConvertFrom-Json).token
  $a = @{ Uri = "https://localhost:$Port$path"; Method = $method; Headers = @{ Authorization = "Bearer $token" }; SkipCertificateCheck = $true }
  if ($body) { $a.Body = ($body | ConvertTo-Json); $a.ContentType = 'application/json' }
  Invoke-RestMethod @a | ForEach-Object { $_ } # a list comes back as one object; hand on its items
}
function Wait([scriptblock] $until, [string] $what, [int] $seconds = 90) {
  $end = (Get-Date).AddSeconds($seconds)
  while ((Get-Date) -lt $end) {
    try { if (& $until) { Write-Host "ok: $what"; return } } catch { }
    Start-Sleep 2
  }
  foreach ($f in "$data\server.log", "$state\agent.log") { if (Test-Path $f) { Write-Host "--- $f"; Get-Content $f -Tail 40 | Write-Host } }
  try { Write-Host '--- agents'; Api '/api/v1/agents' | ConvertTo-Json | Write-Host } catch { Write-Host $_ }
  try { Write-Host '--- bb-agent status'; & "$env:ProgramFiles\better-backup\Agent\bb-agent.exe" status | Write-Host } catch { Write-Host $_ }
  throw "FAILED: $what (waited $seconds seconds)"
}
function Running([string] $name) { (Get-Service $name -ErrorAction SilentlyContinue).Status -eq 'Running' }
function Readers([string] $path) { (Get-Acl $path).Access | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique }

# --- the server ---------------------------------------------------------
Msi @('/i', "`"$Dir\better-backup-server_${Old}_windows_amd64.msi`"", "PORT=$Port", "REPO_DIR=$repo") 'install the server'
Check (Running 'better-backup-server') 'the server service is running'
Wait { (Api '/api/v1/status').version -eq $Old } "the server answers on port $Port as version $Old"
Check (Test-Path "$repo\config.json") "the repository is in $repo"
Check ((Get-NetFirewallRule -DisplayName 'better-backup server' | Get-NetFirewallPortFilter).LocalPort -eq "$Port") 'the firewall rule opens the port'
Check (-not ((Readers $data) -match 'Users')) "ordinary users cannot read $data"
Check ((sc.exe qfailure better-backup-server | Out-String) -match 'RESTART') 'the server service restarts after a failure'
$inst = Api '/api/v1/installers'
$file = $inst.files | Where-Object { $_.os -eq 'windows' -and $_.arch -eq 'amd64' }
Check ($file.name -eq "better-backup-agent_${Old}_windows_amd64.msi") 'the server offers the agent installer'
Invoke-WebRequest "https://localhost:$Port$($file.url)" -SkipCertificateCheck -OutFile "$Dir\downloaded-agent.msi"
Check ((Get-FileHash "$Dir\downloaded-agent.msi").Hash -eq (Get-FileHash "$Dir\$($file.name)").Hash) 'the downloaded agent installer is the one that was built'
Check ((& "$env:ProgramFiles\better-backup\Server\bb.exe" status | Out-String) -match 'version') 'the bb command reaches the server'

# --- the agent, from the installer the server handed out -----------------
$t = Api '/api/v1/enroll-tokens' 'POST' @{ ttl = '1h'; uses = 1 }
$json = @{ s = "https://localhost:$Port"; t = $t.token; f = $t.fingerprint } | ConvertTo-Json -Compress
$code = 'bb1.' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($json)).TrimEnd('=').Replace('+', '-').Replace('/', '_')
Msi @('/i', "`"$Dir\downloaded-agent.msi`"", "ENROLL_CODE=$code", 'AGENT_NAME=msi-test') 'install the agent with an enrollment code'
Check (Running 'better-backup-agent') 'the agent service is running'
Wait { (Api '/api/v1/agents' | Where-Object { $_.name -eq 'msi-test' -and $_.online -and $_.version -eq $Old }) } 'the agent enrolled and is online'
Check (-not (Test-Path "$state\enroll.conf")) 'the used enrollment code was deleted'
$status = & "$env:ProgramFiles\better-backup\Agent\bb-agent.exe" status | Out-String
Write-Host $status
Check ($LASTEXITCODE -eq 0 -and $status -match 'msi-test') 'bb-agent status reaches the service over its local control channel'
# The settings page, as `bb-agent setup` serves it to a browser and as the app shows it.
$agentDir = "$env:ProgramFiles\better-backup\Agent"
$setup = Start-Process "$agentDir\bb-agent.exe" -ArgumentList 'setup', '--no-browser' -RedirectStandardOutput "$Dir\setup.log" -PassThru -WindowStyle Hidden
try {
  Wait { (Get-Content "$Dir\setup.log" -Raw) -match 'http://\S+' } 'bb-agent setup serves the settings page' 20
  $page = [regex]::Match((Get-Content "$Dir\setup.log" -Raw), 'http://\S+').Value
  $web = New-Object Microsoft.PowerShell.Commands.WebRequestSession
  Check ((Invoke-WebRequest $page -WebSession $web).Content -match '<html') 'the settings page loads'
  $shown = Invoke-RestMethod ($page -replace '/\?key=.*', '/api/state') -WebSession $web
  Write-Host ($shown | ConvertTo-Json -Depth 5)
  Check (($shown | ConvertTo-Json -Depth 5) -match 'msi-test') 'the settings page shows this agent'
} finally { Stop-Process -Id $setup.Id -Force -ErrorAction SilentlyContinue }
Check (Test-Path "$env:ProgramData\Microsoft\Windows\Start Menu\Programs\better-backup agent.lnk") 'the settings app is in the Start menu'
$app = Start-Process "$agentDir\bb-agent-ui.exe" -PassThru
Start-Sleep 8
Check (-not $app.HasExited) 'the settings app starts and stays open'
Stop-Process -Id $app.Id -Force

Check (-not ((Readers $state) -match 'Users')) "ordinary users cannot read $state"
Check (-not (Select-String -Path "$Dir\msi-install-the-agent-with-an-enrollment-code.log" -Pattern $t.token -Quiet)) 'the token is not in the installer log'
Check (Test-Path 'HKLM:\SYSTEM\CurrentControlSet\Control\SafeBoot\Network\better-backup-agent') 'the agent may start in safe mode'
$agentId = (Api '/api/v1/agents' | Where-Object name -eq 'msi-test').id

# --- upgrade both ---------------------------------------------------------
Msi @('/i', "`"$Dir\better-backup-server_${New}_windows_amd64.msi`"") 'upgrade the server'
Wait { (Api '/api/v1/status').version -eq $New } "the upgraded server kept port $Port and is version $New"
Check (Test-Path "$repo\config.json") 'the upgraded server kept its repository folder'
Check ((Get-ItemProperty 'HKLM:\SOFTWARE\better-backup\Server').Repo -eq $repo) 'the repository setting survived the upgrade'
Msi @('/i', "`"$Dir\better-backup-agent_${New}_windows_amd64.msi`"") 'upgrade the agent'
Wait { (Api '/api/v1/agents' | Where-Object { $_.id -eq $agentId -and $_.online -and $_.version -eq $New }) } 'the upgraded agent is the same agent, online'
Check (@(Get-Package -ProviderName msi -Name 'better-backup*').Count -eq 2) 'one server and one agent are installed after the upgrade'

# --- removal --------------------------------------------------------------
Msi @('/x', "`"$Dir\better-backup-agent_${New}_windows_amd64.msi`"") 'remove the agent'
Msi @('/x', "`"$Dir\better-backup-server_${New}_windows_amd64.msi`"") 'remove the server'
Check (-not (Get-Service 'better-backup-agent', 'better-backup-server' -ErrorAction SilentlyContinue)) 'both services are gone'
Check (-not (Get-NetFirewallRule -DisplayName 'better-backup server' -ErrorAction SilentlyContinue)) 'the firewall rule is gone'
Check (-not (Test-Path "$env:ProgramFiles\better-backup\Server\bb-server.exe")) 'the programs are gone'
Check ((Test-Path "$data\repo.key") -and (Test-Path "$repo\config.json") -and (Test-Path "$state\agent.json")) 'the backups, keys and agent identity were kept'
Write-Host 'All installer checks passed.'
