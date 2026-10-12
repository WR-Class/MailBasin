' Entry point for the MailBasin scheduled task.
'
' Why this file exists: the task host is the Task Scheduler service, a Windows
' service that predates any agent session, so a process started here cannot be
' inside the agent's job object. That is a structural reason, unlike swapping the
' parent process (WMI), which was tried and did NOT hold up.
'
' Why a .vbs and not powershell.exe: wscript.exe is a GUI-subsystem program, so
' it allocates no console at all. Launching a console program from a task can
' flash a black window; this never does.
'
' Window style 0 = hidden, third argument True = wait for the server to exit,
' which keeps the task alive for exactly as long as the service runs.
Option Explicit

Dim shell, fso, repo, exePath, runDir, stdinFile, outLog, errLog, cmd, stream
Set shell = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")

' This file lives in <repo>\scripts\.
repo = fso.GetParentFolderName(fso.GetParentFolderName(WScript.ScriptFullName))
exePath = fso.BuildPath(repo, "bin\ipm-server.exe")
runDir = fso.BuildPath(repo, "data\run")
stdinFile = fso.BuildPath(runDir, "server.stdin.txt")
outLog = fso.BuildPath(runDir, "server.out.log")
errLog = fso.BuildPath(runDir, "server.err.log")

If Not fso.FileExists(exePath) Then
  WScript.Quit 2
End If
If Not fso.FolderExists(runDir) Then
  If Not fso.FolderExists(fso.GetParentFolderName(runDir)) Then
    fso.CreateFolder(fso.GetParentFolderName(runDir))
  End If
  fso.CreateFolder(runDir)
End If

' Keep one previous generation of the logs; the redirect below truncates them.
If fso.FileExists(outLog) Then fso.CopyFile outLog, outLog & ".prev", True
If fso.FileExists(errLog) Then fso.CopyFile errLog, errLog & ".prev", True

' The binary is an interactive menu; it needs a "1" on stdin to enter serve mode.
Set stream = fso.CreateTextFile(stdinFile, True)
stream.Write "1"
stream.Close

cmd = "cmd /c cd /d """ & repo & """ && """ & exePath & """ < """ & stdinFile & """ > """ & outLog & """ 2> """ & errLog & """"
shell.Run cmd, 0, True
