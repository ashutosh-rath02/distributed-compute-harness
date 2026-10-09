# MSI (WiX) is not built

An MSI installer needs the WiX toolset, which is not installed on the build machine
used so far, so no `.wxs` sketch is included rather than an untested one. Until then
Windows is covered by:

- the bundle's `install.ps1` (manager) and the dashboard's join script (agent),
- `uninstall.ps1` in this folder (tested against a throwaway install),
- the winget manifest templates in `winget/` (portable commands only; not validated).

If you add an MSI later, make its uninstall remove the same scheduled tasks
(`HomeComputeHarnessAgent`, `HomeComputeHarnessManager`) that `uninstall.ps1` does.
