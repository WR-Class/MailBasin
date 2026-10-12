@echo off
rem MailBasin service control. The real logic lives in server.ps1.
rem
rem   scripts\server.cmd start     start
rem   scripts\server.cmd stop      stop
rem   scripts\server.cmd restart   stop then start
rem   scripts\server.cmd status    PID, ancestor chain, HTTP probe (default)
rem
rem The service is launched via WMI, so its parent is WmiPrvSE.exe and it is
rem NOT in the DSH process tree. Restarting DSH will not take it down.
rem Do not change this back to Start-Process.
rem
rem Keep this file ASCII-only: cmd.exe reads .cmd in the OEM code page (936 here),
rem so non-ASCII text in it gets mangled and can split lines.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0server.ps1" %*
exit /b %ERRORLEVEL%
