# Removes Home Harness from this Windows PC: the agent, the manager, or both.
# It undoes exactly what the join script and scripts\windows-manager\install.ps1
# set up: it stops and deletes their scheduled tasks (and the older Run-key
# fallback), stops their launcher loops and programs, and deletes their
# program folders. Nothing needs admin rights; it runs as you.
#
#   powershell -ExecutionPolicy Bypass -File uninstall.ps1                   (agent and manager, asks first)
#   powershell -ExecutionPolicy Bypass -File uninstall.ps1 -Component agent
#   powershell -ExecutionPolicy Bypass -File uninstall.ps1 -Component manager -RemoveState
#
# By default it KEEPS your data, so a later reinstall picks up where you left off:
#   - the agent's identity (the device's name and keys with the manager), and
#   - the manager's state (database, operator token, certificate, stored files).
# -RemoveState deletes those too. Deleting the manager's state is permanent:
# devices that trusted that manager will not trust a new one. Export it first
# if you might want it back:  manager.exe -export-state state.zip -state-dir <folder>
#
# Run it with -WhatIf to see what it would do without doing anything.
# Run it with -Yes to skip the question (for scripts).
[CmdletBinding(SupportsShouldProcess = $true)]
param(
  [ValidateSet("both", "agent", "manager")][string]$Component = "both",
  [switch]$RemoveState,
  [switch]$Yes,
  # Where things are. The defaults are where the installers put them; the
  # parameters exist for non-standard installs and for testing.
  [string]$AgentDir = (Join-Path $env:LOCALAPPDATA "HomeHarness"),
  [string]$ManagerDir = (Join-Path $env:LOCALAPPDATA "HomeHarness\manager"),
  [string]$IdentityDir = (Join-Path $env:APPDATA "home-harness\agent"),
  [string]$AgentTaskName = "HomeComputeHarnessAgent",
  [string]$ManagerTaskName = "HomeComputeHarnessManager"
)
$ErrorActionPreference = "Stop"

function Say([string]$m) { Write-Host $m }

# Refuse paths that are plainly not an install folder: a typo here would
# otherwise delete something of yours.
function Assert-SafeDir([string]$p, [string]$what) {
  # Compared in 8.3 short form, so C:\Users\ASHUTO~1 and C:\Users\Ashutosh Rath are the same folder.
  function Short([string]$x) {
    $x = [IO.Path]::GetFullPath($x).TrimEnd('\')
    try { return (New-Object -ComObject Scripting.FileSystemObject).GetFolder($x).ShortPath.TrimEnd('\') } catch { return $x }
  }
  $full = Short $p
  $roots = @($env:USERPROFILE, $env:LOCALAPPDATA, $env:APPDATA, $env:SystemDrive + "\", $env:TEMP) | ForEach-Object { if ($_) { Short $_ } }
  if ($roots -contains $full -or $full.Length -lt 12) { throw "Refusing to delete $what '$full': it is not a Home Harness folder." }
}

# Stop and remove one install's scheduled task, Run-key fallback, launcher
# loop and program. Only processes whose path is inside $dir are touched.
function Remove-Install([string]$label, [string]$taskName, [string]$dir, [string]$exeName, [string]$launcherName) {
  Say "-- $label"
  $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
  if ($task) {
    if ($PSCmdlet.ShouldProcess($taskName, "stop and delete the scheduled task")) {
      Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
      Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
      Say "   removed scheduled task $taskName"
    }
  } else {
    Say "   no scheduled task named $taskName"
  }
  $runKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
  if (Get-ItemProperty -Path $runKey -Name $taskName -ErrorAction SilentlyContinue) {
    if ($PSCmdlet.ShouldProcess($taskName, "remove the sign-in (Run key) entry")) {
      Remove-ItemProperty -Path $runKey -Name $taskName -ErrorAction SilentlyContinue
      Say "   removed the Run-key entry $taskName"
    }
  }
  if (-not (Test-Path -LiteralPath $dir)) { Say "   no folder $dir"; return }
  $dirFull = [IO.Path]::GetFullPath($dir).TrimEnd('\') + '\'
  $launcher = Join-Path $dir $launcherName
  # The launcher loop first (it would restart the program), then the program.
  Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -like ('*' + $launcher + '*') } |
    ForEach-Object { if ($PSCmdlet.ShouldProcess("launcher (pid $($_.ProcessId))", "stop")) { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue } }
  Get-Process ($exeName -replace '\.exe$', '') -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -and ([IO.Path]::GetFullPath($_.Path)).StartsWith($dirFull, [StringComparison]::OrdinalIgnoreCase) } |
    ForEach-Object { if ($PSCmdlet.ShouldProcess("$exeName (pid $($_.Id))", "stop")) { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue } }
  Start-Sleep -Milliseconds 500
}

$doAgent = $Component -in @("both", "agent")
$doManager = $Component -in @("both", "manager")

Say "This will remove Home Harness from this PC ($Component)."
if ($doAgent)   { Say ("  agent:   task $AgentTaskName, folder $AgentDir" + $(if ($RemoveState) { ", identity $IdentityDir" } else { " (identity kept)" })) }
if ($doManager) { Say ("  manager: task $ManagerTaskName, folder $ManagerDir" + $(if ($RemoveState) { " INCLUDING its state (database, tokens, certificate): permanent" } else { " (its state folder is kept)" })) }
if (-not $Yes -and -not $WhatIfPreference) {
  if (-not [Environment]::UserInteractive -or [Console]::IsInputRedirected) { throw "Not asking in a non-interactive session: add -Yes to go ahead." }
  $answer = Read-Host "Type yes to continue"
  if ($answer -ne "yes") { Say "Nothing was changed."; return }
}

# The manager first: when both are installed in the default places its
# folder is inside the agent's, and its state is the part worth keeping.
if ($doManager) {
  Assert-SafeDir $ManagerDir "the manager folder"
  Remove-Install "manager" $ManagerTaskName $ManagerDir "manager.exe" "run-manager.ps1"
  if (Test-Path -LiteralPath $ManagerDir) {
    $stateDir = (Join-Path $ManagerDir "state").TrimEnd('\')
    Get-ChildItem -LiteralPath $ManagerDir -Force | ForEach-Object {
      # By name: Get-ChildItem may spell the path in long form where the parameter used the short one.
      if (-not $RemoveState -and $_.Name -eq "state") { return }
      if ($PSCmdlet.ShouldProcess($_.FullName, "delete")) { Remove-Item -LiteralPath $_.FullName -Recurse -Force }
    }
    if ($RemoveState) {
      if ($PSCmdlet.ShouldProcess($ManagerDir, "delete the folder")) { Remove-Item -LiteralPath $ManagerDir -Recurse -Force -ErrorAction SilentlyContinue }
    } elseif (Test-Path -LiteralPath $stateDir) {
      Say "   kept the manager's state in $stateDir (delete the folder yourself, or run again with -RemoveState, to remove it)"
    }
  }
}

if ($doAgent) {
  Assert-SafeDir $AgentDir "the agent folder"
  Remove-Install "agent" $AgentTaskName $AgentDir "agent.exe" "start-agent.ps1"
  $mgrFull = [IO.Path]::GetFullPath($ManagerDir).TrimEnd('\')
  Get-ChildItem -LiteralPath $AgentDir -Force -ErrorAction SilentlyContinue | ForEach-Object {
    # A manager folder still here (its state kept, or not being removed) stays.
    # (compared by name: Get-ChildItem may spell the path in long form where the parameter used the short one)
    if ($_.PSIsContainer -and $_.Name -eq (Split-Path -Leaf $mgrFull) -and (Split-Path -Leaf (Split-Path -Parent $mgrFull)) -eq (Split-Path -Leaf $AgentDir)) { return }
    if ($PSCmdlet.ShouldProcess($_.FullName, "delete")) { Remove-Item -LiteralPath $_.FullName -Recurse -Force }
  }
  if ((Test-Path -LiteralPath $AgentDir) -and -not (Get-ChildItem -LiteralPath $AgentDir -Force -ErrorAction SilentlyContinue)) {
    if ($PSCmdlet.ShouldProcess($AgentDir, "delete the empty folder")) { Remove-Item -LiteralPath $AgentDir -Force -ErrorAction SilentlyContinue }
  }
  if ($RemoveState -and (Test-Path -LiteralPath $IdentityDir)) {
    Assert-SafeDir $IdentityDir "the agent identity folder"
    if ($PSCmdlet.ShouldProcess($IdentityDir, "delete the agent identity")) { Remove-Item -LiteralPath $IdentityDir -Recurse -Force; Say "   deleted the agent identity $IdentityDir" }
  }
}

Say ""
Say "Done. If Windows Firewall has rules named agent.exe or manager.exe, they are harmless leftovers:"
Say "remove them in Windows Security > Firewall > Allowed apps if you like."
