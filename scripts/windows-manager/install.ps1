# Installs (or updates) the Home Compute Harness manager on this Windows PC:
# copies it with the agent builds it hands out, keeps its state (identity,
# tokens, database) across updates, and starts it at every sign-in through a
# per-user scheduled task that restarts it within seconds if it ever exits.
# No admin rights needed; it runs as you.
#
#   install.cmd                       (double-click), or
#   powershell -ExecutionPolicy Bypass -File install.ps1 [-InstallDir DIR] [-ImportState state.zip] [-NoDashboard]
#
# -ImportState brings in a manager state exported elsewhere (manager
# -export-state), so devices that trust that manager keep trusting this one.
param(
  [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "HomeHarness\manager"),
  [string]$ImportState = "",
  [switch]$NoDashboard
)
$ErrorActionPreference = "Stop"
$src = $PSScriptRoot
$taskName = "HomeComputeHarnessManager"
$manager = Join-Path $InstallDir "manager.exe"
$launcher = Join-Path $InstallDir "run-manager.ps1"
$state = Join-Path $InstallDir "state"
$agents = Join-Path $InstallDir "agents"
New-Item -ItemType Directory -Force $InstallDir, $state, $agents | Out-Null

# Stop a manager installed here earlier: its task, its launcher loop and the
# manager itself. Only this install's (matched by path): an agent on this PC
# keeps running.
Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" | Where-Object { $_.CommandLine -like ('*' + $launcher + '*') } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Get-Process manager -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $manager } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 800

# The files. Agent builds are mirrored, so a platform dropped from this
# bundle stops being served.
if ((Resolve-Path $src).Path -ne (Resolve-Path $InstallDir).Path) {
  Copy-Item (Join-Path $src "manager.exe") $manager -Force
  Copy-Item (Join-Path $src "harnessctl.exe") (Join-Path $InstallDir "harnessctl.exe") -Force
  Get-ChildItem $agents -File | Remove-Item -Force
  Copy-Item (Join-Path $src "agents\*") $agents -Force
  $apk = Join-Path $src "home-harness.apk"
  if (Test-Path $apk) { Copy-Item $apk (Join-Path $InstallDir "home-harness.apk") -Force }
}

if ($ImportState) {
  & $manager -import-state $ImportState -state-dir $state -force
  if ($LASTEXITCODE -ne 0) { throw "importing the state failed (see above)" }
}
$tokenFile = Join-Path $state "pairing-token"
if (-not (Test-Path $tokenFile)) {
  $b = New-Object byte[] 32
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
  (($b | ForEach-Object { $_.ToString("x2") }) -join "") | Set-Content -Encoding ascii -NoNewline $tokenFile
}

# The launcher: runs the manager and starts it again 5 seconds after it
# exits, for as long as you're signed in.
@'
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$state = Join-Path $dir "state"
function Q([string]$p) { '"' + $p + '"' }
$exe = Join-Path $dir "manager.exe"
# This loop restarts the manager: one that needs a restart (a standby
# promoted after stepping down) just exits instead of starting itself.
$env:HOME_HARNESS_SUPERVISED = "1"
while ($true) {
  # Only this loop starts the manager (the task never runs two loops), so
  # one already running from this folder was left behind by a loop that
  # was stopped: replace it, or the new one couldn't get the ports.
  Get-Process manager -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe } | Stop-Process -Force -ErrorAction SilentlyContinue
  $token = (Get-Content (Join-Path $state "pairing-token") -Raw).Trim()
  $a = @("-addr", ":7420", "-api-addr", "127.0.0.1:7421", "-join-addr", ":7419", "-pairing-token", $token,
         "-operator-token-file", (Q (Join-Path $state "operator-token")), "-db", (Q (Join-Path $state "manager.db")),
         "-tls-dir", (Q (Join-Path $state "tls")), "-artifact-dir", (Q (Join-Path $state "artifacts")))
  Get-ChildItem (Join-Path $dir "agents") -File | ForEach-Object { $a += @("-agent-binary", (Q $_.FullName)) }
  $apk = Join-Path $dir "home-harness.apk"
  if (Test-Path $apk) { $a += @("-app-apk", (Q $apk)) }
  $log = Join-Path $state "manager.log"
  if (Test-Path $log) { Move-Item -Force $log (Join-Path $state "manager.prev.log") }
  Start-Process -FilePath $exe -ArgumentList $a -WorkingDirectory $dir -WindowStyle Hidden -RedirectStandardError $log -RedirectStandardOutput (Join-Path $state "manager.out.log") -Wait
  Start-Sleep -Seconds 5
}
'@ | Set-Content -Encoding UTF8 $launcher

# Start at sign-in: a per-user task, never elevated, kept running on
# battery, restarted if the launcher itself ever stops.
$user = "$env:USERDOMAIN\$env:USERNAME"
$launchArgs = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $launcher + '"'
$action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument $launchArgs
# At sign-in, and every 2 minutes as a watchdog: "restart on failure" only
# covers a launch that fails, so a launcher that stops later would stay
# down until the next sign-in. While it runs, extra starts are ignored.
$trigger = @((New-ScheduledTaskTrigger -AtLogOn -User $user), (New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(2) -RepetitionInterval (New-TimeSpan -Minutes 2)))
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Description "Home Compute Harness manager" -Force | Out-Null
Start-ScheduledTask -TaskName $taskName

# Wait for it to come up, then show how to reach it.
$log = Join-Path $state "manager.log"
$link = ""
for ($i = 0; $i -lt 60 -and -not $link; $i++) {
  Start-Sleep -Milliseconds 500
  if (Test-Path $log) {
    $m = Select-String -Path $log -Pattern "#login=[0-9a-f]+" | Select-Object -Last 1
    if ($m) { $link = ($m.Line -split " ")[-1] }
  }
}
if (-not $link) { throw "the manager didn't start; see $log" }
$fp = (Get-Content $log | Select-String -Pattern "^\s+[0-9a-f]{64}\s*$" | Select-Object -First 1)
Write-Host ""
Write-Host "Home Harness manager installed in $InstallDir and running."
Write-Host "It starts by itself whenever you sign in, and restarts if it ever stops."
if ($fp) { Write-Host ("Certificate fingerprint: " + $fp.Line.Trim()) }
Write-Host "Dashboard: http://127.0.0.1:7421 (sign-in link: $link)"
Write-Host "If Windows asks whether manager.exe may accept network connections, allow it on private networks, so your devices can reach it."
if (-not $NoDashboard) { Start-Process $link }
