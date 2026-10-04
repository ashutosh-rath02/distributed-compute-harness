# Installs (or updates) the Home Compute Harness manager on this Windows PC:
# copies it with the agent builds it hands out, keeps its state (identity,
# tokens, database) across updates, and starts it at every sign-in through a
# per-user scheduled task that restarts it within seconds if it ever exits.
# No admin rights needed; it runs as you.
#
#   install.cmd                       (double-click), or
#   powershell -ExecutionPolicy Bypass -File install.ps1 [-InstallDir DIR] [-ImportState state.zip] [-ReleaseKey KEY] [-VerifyOnly] [-NoDashboard]
#
# -ImportState brings in a manager state exported elsewhere (manager
# -export-state), so devices that trust that manager keep trusting this one.
#
# A bundle with a signed release manifest (SHA256SUMS) is checked first,
# before the running manager is stopped or any file replaced: a bundle
# changed since it was built doesn't install. The key it must be signed
# with: -ReleaseKey (base64, or a release-key.pub you got separately) if
# given, else the one this PC pinned when it installed a signed bundle
# (release-key.pub in the install folder), else, the first time, the
# bundle's own, which proves only that the bundle is intact. Once a signed
# bundle is installed, unsigned bundles and other keys' are refused.
# -VerifyOnly checks the bundle and stops there. This guards the bundle,
# not Windows' view of it: SmartScreen still warns about programs that
# aren't Authenticode-signed with a code-signing certificate.
param(
  [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "HomeHarness\manager"),
  [string]$ImportState = "",
  [string]$ReleaseKey = "",
  [switch]$VerifyOnly,
  [switch]$NoDashboard
)
$ErrorActionPreference = "Stop"
$src = $PSScriptRoot
$taskName = "HomeComputeHarnessManager"
$manager = Join-Path $InstallDir "manager.exe"
$launcher = Join-Path $InstallDir "run-manager.ps1"
$state = Join-Path $InstallDir "state"
$agents = Join-Path $InstallDir "agents"

# The release check (see above), run by the bundle's own manager.exe with
# the same code the build vetted it with.
$pinned = Join-Path $InstallDir "release-key.pub"
$sums = Join-Path $src "SHA256SUMS"
$key = ""
if ($ReleaseKey) {
  $key = $ReleaseKey
  if (Test-Path -LiteralPath $ReleaseKey -PathType Leaf) { $key = (Get-Content -LiteralPath $ReleaseKey -Raw).Trim() }
} elseif (Test-Path -LiteralPath $pinned) {
  $key = (Get-Content -LiteralPath $pinned -Raw).Trim()
}
if (Test-Path -LiteralPath $sums) {
  $bundleManager = Join-Path $src "manager.exe"
  if (-not (Test-Path -LiteralPath $bundleManager)) { throw "This bundle has no manager.exe. Nothing was installed or stopped." }
  if (-not $key) {
    $bundleKey = Join-Path $src "release-key.pub"
    if (-not (Test-Path -LiteralPath $bundleKey)) { throw "This bundle has a release manifest but no release-key.pub. Nothing was installed or stopped." }
    $key = (Get-Content -LiteralPath $bundleKey -Raw).Trim()
    Write-Host "First signed bundle on this PC: checking it with its own release key, which is pinned for later updates."
  }
  $check = @("-check-agent-binaries", "-release-key", $key, "-release-manifest", $sums)
  Get-ChildItem (Join-Path $src "agents") -File | ForEach-Object { $check += @("-agent-binary", $_.FullName) }
  $bundleApk = Join-Path $src "home-harness.apk"
  if (Test-Path -LiteralPath $bundleApk) { $check += @("-app-apk", $bundleApk) }
  # Its verdict comes on stderr: collect it, rather than let Windows
  # PowerShell turn the first line into an exception of its own.
  $ErrorActionPreference = "Continue"
  $out = & $bundleManager @check 2>&1 | ForEach-Object { "$_" }
  $code = $LASTEXITCODE
  $ErrorActionPreference = "Stop"
  if ($code -ne 0) {
    $out | ForEach-Object { Write-Host $_ }
    throw "This bundle failed its release check (above): it was changed after it was built, or signed with another release key. Nothing was installed or stopped. If the release key was replaced on purpose, pass -ReleaseKey with the new release-key.pub, or delete $pinned."
  }
  $out | Where-Object { $_ -like "release:*" } | ForEach-Object { Write-Host $_ }
} elseif ($key) {
  throw "This bundle has no signed release manifest (SHA256SUMS), but this PC takes only signed bundles (-ReleaseKey, or the key pinned in $pinned). Nothing was installed or stopped. To install an unsigned bundle anyway, delete $pinned first."
} else {
  Write-Host "This bundle has no signed release manifest (SHA256SUMS): installing it unchecked."
}
if ($VerifyOnly) {
  Write-Host "Checked only (-VerifyOnly): nothing was installed or stopped."
  return
}
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
# Pin the key a signed bundle was checked with: from now on, only bundles
# signed with it install here.
if ((Test-Path -LiteralPath $sums) -and $key) { Set-Content -LiteralPath $pinned -Value $key -Encoding ascii }

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
