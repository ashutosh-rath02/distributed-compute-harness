# Tests uninstall.ps1 against a THROWAWAY install under %TEMP% and scheduled
# tasks with test-only names. It never touches the real install or the real
# task names: it refuses to run if a name it would use is a real one.
#
#   powershell -ExecutionPolicy Bypass -File packaging\windows\test-uninstall.ps1
$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$uninstall = Join-Path $here "uninstall.ps1"
$stamp = [Guid]::NewGuid().ToString("N").Substring(0, 8)
$agentTask = "HHUninstallTest-Agent-$stamp"
$managerTask = "HHUninstallTest-Manager-$stamp"
if ($agentTask -in @("HomeComputeHarnessAgent", "HomeComputeHarnessManager") -or $managerTask -in @("HomeComputeHarnessAgent", "HomeComputeHarnessManager")) { throw "refusing: real task name" }
$base = Join-Path $env:TEMP "docs-track\uninstall-test-$stamp"
$failures = 0
function Check([bool]$ok, [string]$what) { if ($ok) { Write-Host "ok    $what" } else { Write-Host "FAIL  $what"; $script:failures++ } }

function New-Throwaway {
  Remove-Item -LiteralPath $base -Recurse -Force -ErrorAction SilentlyContinue
  $script:agentDir = Join-Path $base "LocalAppData\HomeHarness"
  $script:managerDir = Join-Path $script:agentDir "manager"
  $script:identity = Join-Path $base "Roaming\home-harness\agent"
  New-Item -ItemType Directory -Force (Join-Path $script:managerDir "state"), (Join-Path $script:managerDir "agents"), $script:identity | Out-Null
  Set-Content (Join-Path $script:agentDir "agent.exe") "x"; Set-Content (Join-Path $script:agentDir "start-agent.ps1") "x"; Set-Content (Join-Path $script:agentDir "agent.log") "x"
  Set-Content (Join-Path $script:managerDir "manager.exe") "x"; Set-Content (Join-Path $script:managerDir "run-manager.ps1") "x"
  Set-Content (Join-Path $script:managerDir "state\operator-token") "secret-test-token"
  Set-Content (Join-Path $script:identity "identity.json") "x"
  foreach ($t in @($agentTask, $managerTask)) {
    $action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c exit"
    $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddYears(1)
    Register-ScheduledTask -TaskName $t -Action $action -Trigger $trigger -Description "throwaway test of uninstall.ps1" -Force | Out-Null
  }
}
function Run([string[]]$extra) {
  & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $uninstall -AgentDir $script:agentDir -ManagerDir $script:managerDir -IdentityDir $script:identity -AgentTaskName $agentTask -ManagerTaskName $managerTask @extra | Out-Host
  if ($LASTEXITCODE -ne 0) { Write-Host "uninstall exited $LASTEXITCODE" }
}
function HasTask($n) { [bool](Get-ScheduledTask -TaskName $n -ErrorAction SilentlyContinue) }

try {
  # 1. -WhatIf changes nothing.
  New-Throwaway
  Run @("-Yes", "-WhatIf")
  Check (HasTask $agentTask) "WhatIf keeps the agent task"
  Check (Test-Path (Join-Path $agentDir "agent.exe")) "WhatIf keeps the agent files"

  # 2. Without -Yes in a non-interactive run it refuses and changes nothing.
  Run @()
  Check (HasTask $agentTask -and (Test-Path (Join-Path $agentDir "agent.exe"))) "no -Yes: refuses, nothing removed"

  # 3. Agent only: agent gone, manager and identity kept.
  Run @("-Component", "agent", "-Yes")
  Check (-not (HasTask $agentTask)) "agent task removed"
  Check (HasTask $managerTask) "manager task untouched by -Component agent"
  Check (-not (Test-Path (Join-Path $agentDir "agent.exe"))) "agent program removed"
  Check (Test-Path (Join-Path $managerDir "manager.exe")) "manager folder kept (inside the agent folder)"
  Check (Test-Path (Join-Path $identity "identity.json")) "identity kept without -RemoveState"

  # 4. Fresh: both, keep state.
  New-Throwaway
  Run @("-Yes")
  Check (-not (HasTask $agentTask) -and -not (HasTask $managerTask)) "both tasks removed"
  Check (-not (Test-Path (Join-Path $managerDir "manager.exe"))) "manager program removed"
  Check (Test-Path (Join-Path $managerDir "state\operator-token")) "manager state kept without -RemoveState"
  Check (Test-Path (Join-Path $identity "identity.json")) "identity kept"

  # 5. Fresh: both with -RemoveState removes everything.
  New-Throwaway
  Run @("-Yes", "-RemoveState")
  Check (-not (Test-Path $agentDir)) "program folders gone with -RemoveState"
  Check (-not (Test-Path $identity)) "identity gone with -RemoveState"

  # 6. Running it again when nothing is left is fine.
  Run @("-Yes")
  Check ($true) "second run on an empty install does not fail"

  # 7. A silly path is refused.
  $ErrorActionPreference = "Continue"
  & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $uninstall -AgentDir $env:USERPROFILE -AgentTaskName $agentTask -ManagerTaskName $managerTask -Component agent -Yes 2>&1 | Out-Null
  $code = $LASTEXITCODE
  $ErrorActionPreference = "Stop"
  Check ($code -ne 0 -and (Test-Path $env:USERPROFILE)) "refuses a folder that is not an install folder"
}
finally {
  foreach ($t in @($agentTask, $managerTask)) { Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue }
  Remove-Item -LiteralPath $base -Recurse -Force -ErrorAction SilentlyContinue
}
if ($failures -gt 0) { Write-Host "$failures check(s) failed"; exit 1 }
Write-Host "all checks passed"
