' Entry point for the MailBasin-Update scheduled task.
'
' Same two structural reasons as server-run-hidden.vbs:
'   - The task host is the Task Scheduler service, a Windows service that predates
'     any agent session, so a process started here cannot be inside the agent's job
'     object. Swapping the parent process (WMI) was tried and did NOT hold up.
'   - wscript.exe is a GUI-subsystem program and allocates no console, so the update
'     never flashes a black window.
'
' Why a second task rather than reusing MailBasin: the updater has to stop, replace
' and restart the service. If it ran inside that task, stopping the service would
' stop the updater mid-swap. Two independent tasks keep them separable.
'
' Why the third argument is True: this waits for the update to finish, so the task
' stays active for exactly as long as the update runs. That makes the task
' scheduler's single-instance rule a real second guard against concurrent updates.
' The exit code is propagated so a failed update shows up in the task history too.
Option Explicit

Dim shell, fso, repo, scriptPath, runDir, cmd, exitCode
Set shell = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")

' This file lives in <repo>\scripts\.
repo = fso.GetParentFolderName(fso.GetParentFolderName(WScript.ScriptFullName))
scriptPath = fso.BuildPath(repo, "scripts\update.ps1")
runDir = fso.BuildPath(repo, "data\run")

If Not fso.FileExists(scriptPath) Then
  WScript.Quit 2
End If

If Not fso.FolderExists(runDir) Then
  If Not fso.FolderExists(fso.GetParentFolderName(runDir)) Then
    fso.CreateFolder(fso.GetParentFolderName(runDir))
  End If
  fso.CreateFolder(runDir)
End If

cmd = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File """ & scriptPath & """"
exitCode = shell.Run(cmd, 0, True)
WScript.Quit exitCode
