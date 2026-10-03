@echo off
rem Installs or updates the Home Harness manager on this PC (no admin needed).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
pause
