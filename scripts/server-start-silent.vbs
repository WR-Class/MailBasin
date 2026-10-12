' Entry point for the desktop shortcut: ask Task Scheduler to start MailBasin.
'
' Hidden (window style 0) so double-clicking the shortcut shows nothing at all.
' The task is what actually hosts the service -- see server-run-hidden.vbs.
Option Explicit

Dim shell, fso, repo, ps1
Set shell = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")

' This file lives in <repo>\scripts\.
repo = fso.GetParentFolderName(fso.GetParentFolderName(WScript.ScriptFullName))
ps1 = fso.BuildPath(repo, "scripts\server.ps1")

shell.Run "powershell.exe -NoProfile -ExecutionPolicy Bypass -File """ & ps1 & """ start", 0, False
