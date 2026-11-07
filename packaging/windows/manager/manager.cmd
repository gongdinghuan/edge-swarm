@echo off
setlocal
"%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0manager.ps1" %*
set "manager_status=%errorlevel%"
if not "%manager_status%"=="0" if "%~1"=="" pause
exit /b %manager_status%
