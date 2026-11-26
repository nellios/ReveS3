@echo off
setlocal EnableExtensions DisableDelayedExpansion
"%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0Start-Gateway.ps1" -LoginOnly
set "gateway_exit=%errorlevel%"
echo.
pause
exit /b %gateway_exit%
