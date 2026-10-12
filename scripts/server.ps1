<#
.SYNOPSIS
  MailBasin 服务的启停入口：start / stop / restart / status / install-task / uninstall-task。

.DESCRIPTION
  这是「怎么启动这个服务」的单一出处。以后要重启它，用这个脚本，
  不要由 agent 临场拼命令。

  服务由计划任务 MailBasin 托管。任务由计划任务服务（svchost）承载，那个服务
  早于 DSH 存在，不可能落在 DSH 的作业里——这是结构上的理由。

  为什么不用 WMI 换父进程：试过，被实测证伪。WMI（Win32_Process.Create）能把
  父进程换成 WmiPrvSE.exe，祖先链当时也验证过（ipm-server ← cmd ← WmiPrvSE ←
  svchost ← services ← wininit，链上确实没有 DSH），但 DSH 一重启服务照样被带走——
  它自己的日志留下了关闭记录。换父进程不等于脱离，祖先链干净不是安全证据。

  start 只是触发那个任务；服务不是这个脚本的孩子，也不在 DSH 的树下。
  install-task 注册任务并放一个桌面快捷方式，那是新机器上的第一步。

.PARAMETER Action
  start          触发计划任务启动（已在运行则什么都不做）
  stop           停止
  restart        先停后启
  status         打印 PID、祖先链和一次 HTTP 探测（默认动作）
  install-task   注册计划任务 + 创建桌面快捷方式
  uninstall-task 注销计划任务 + 删除桌面快捷方式

.PARAMETER Port
  服务监听端口，默认 8788。

.EXAMPLE
  scripts\server.cmd install-task

.EXAMPLE
  scripts\server.cmd restart
#>
[CmdletBinding()]
param(
  [Parameter(Position = 0)]
  [ValidateSet('start', 'stop', 'restart', 'status', 'install-task', 'uninstall-task')]
  [string]$Action = 'status',

  [int]$Port = 8788,

  [int]$WaitSeconds = 40
)

$ErrorActionPreference = 'Stop'

# 仓库根：本脚本在 <root>\scripts\ 下。
$RepoRoot  = Split-Path -Parent $PSScriptRoot
$Exe       = Join-Path $RepoRoot 'bin\ipm-server.exe'
$RunDir    = Join-Path $RepoRoot 'data\run'        # data/ 已在 .gitignore 里，不会污染仓库
$StdinFile = Join-Path $RunDir 'server.stdin.txt'
$OutLog    = Join-Path $RunDir 'server.out.log'
$ErrLog    = Join-Path $RunDir 'server.err.log'

# 服务由计划任务托管。任务由计划任务服务（svchost）承载，那个服务早于 agent 存在，
# 不可能落在 agent 的作业里——这是结构上的理由，比「换成别的父进程」强。
# 之前用 WMI 换父进程的做法已被实测证伪：祖先链上确实没有 agent，服务照样在
# DSH 重启时被带走（日志显示它干净退出）。
$TaskName  = 'MailBasin'
$TaskVbs   = Join-Path $PSScriptRoot 'server-run-hidden.vbs'
$StartVbs  = Join-Path $PSScriptRoot 'server-start-silent.vbs'

# agent 自己的进程名。祖先链上出现任何一个，都说明这次启动没摘干净。
$AgentProcessNames = @('DeepSeek Harness.exe', 'electron.exe')

function Get-ServerProcessId {
  $conn = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
  if (-not $conn) { return $null }
  return ($conn | Select-Object -First 1).OwningProcess
}

function Get-AncestorChain {
  param([int]$StartPid, [int]$Depth = 8)
  $chain = @()
  $cur = $StartPid
  for ($i = 0; $i -lt $Depth; $i++) {
    $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $cur" -ErrorAction SilentlyContinue
    if (-not $proc) { break }
    $chain += [pscustomobject]@{ Pid = $proc.ProcessId; Name = $proc.Name; Parent = $proc.ParentProcessId }
    if (-not $proc.ParentProcessId -or $proc.ParentProcessId -eq $cur) { break }
    $cur = $proc.ParentProcessId
  }
  return $chain
}

function Show-Chain {
  param([int]$StartPid)
  Write-Host "`n祖先链（DSH 的进程不该出现在这里）："
  $chain = Get-AncestorChain -StartPid $StartPid
  $depth = 0
  foreach ($node in $chain) {
    Write-Host ("  " * $depth + "PID $($node.Pid)  $($node.Name)")
    $depth++
  }
  $agent = $chain | Where-Object { $AgentProcessNames -contains $_.Name } | Select-Object -First 1
  if ($agent) {
    Write-Host "  [没摘干净] 链上出现了 agent 进程 $($agent.Name) (PID $($agent.Pid))" -ForegroundColor Red
    return $false
  }
  # 链走到服务宿主算走完。中途断掉是正常的——WMI 提供程序宿主空闲后会被回收，
  # 那时 cmd.exe 就成了孤儿——但断了就无法再从链上证明它通向哪里，
  # 所以如实说明，不假装验证过。刚启动时链是完整的。
  $last = $chain[-1]
  if ($last.Name -match '^(svchost|services|wininit|WmiPrvSE)\.exe$') {
    Write-Host "  [已摘干净] 链上没有 agent 进程，且通向 $($last.Name)" -ForegroundColor Green
    return $true
  }
  Write-Host "  [已摘干净，但链在此断开] 没有 agent 进程；PID $($last.Pid) 的父进程已退出，" -ForegroundColor Yellow
  Write-Host "  无法再从链上证明它通向哪里（WMI 提供程序宿主空闲后会被回收，属正常）。" -ForegroundColor Yellow
  return $true
}

function Show-LogTail {
  param([string]$Path, [int]$Lines = 30)
  if (-not (Test-Path $Path)) { return }
  Write-Host "`n--- $Path 末尾 ---"
  Get-Content $Path -Tail $Lines | ForEach-Object { Write-Host "  $_" }
}

function Test-TaskRegistered {
  return [bool](Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue)
}

function Invoke-Start {
  $existing = Get-ServerProcessId
  if ($existing) {
    Write-Host "服务已在运行，PID $existing（端口 $Port）。要重启请用 restart。"
    return
  }
  if (-not (Test-TaskRegistered)) {
    throw "计划任务 $TaskName 还没注册。先跑一次：scripts\server.cmd install-task"
  }
  # 触发任务，服务由计划任务服务托管——不经过 DSH，也不经过这个脚本。
  Start-ScheduledTask -TaskName $TaskName

  $serverPid = $null
  $deadline = (Get-Date).AddSeconds($WaitSeconds)
  while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 500
    $serverPid = Get-ServerProcessId
    if ($serverPid) { break }
  }
  if (-not $serverPid) {
    Show-LogTail -Path $ErrLog
    throw "等了 $WaitSeconds 秒，端口 $Port 仍未监听。错误日志在上面。"
  }
  Write-Host "服务已启动，PID $serverPid"
  [void](Show-Chain -StartPid $serverPid)
}

function Invoke-Stop {
  $serverPid = Get-ServerProcessId
  if (-not $serverPid) {
    Write-Host "服务未在运行，无需停止。"
    return
  }
  Stop-Process -Id $serverPid -Force
  $deadline = (Get-Date).AddSeconds(15)
  while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 300
    if (-not (Get-ServerProcessId)) { break }
  }
  if (Get-ServerProcessId) { throw "PID $serverPid 已发停止信号，但端口 $Port 仍在监听。" }
  Write-Host "服务已停止（原 PID $serverPid）。"
}

function Invoke-Status {
  $serverPid = Get-ServerProcessId
  if (-not $serverPid) {
    Write-Host "服务未在运行（端口 $Port 没有监听）。启动：scripts\server.cmd start"
    return
  }
  Write-Host "服务运行中，PID $serverPid，端口 $Port"
  [void](Show-Chain -StartPid $serverPid)
  # 进程还在不等于它在服务，所以再打一次端点。
  try {
    $resp = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/" -UseBasicParsing -TimeoutSec 10
    Write-Host "  [应答正常] HTTP $($resp.StatusCode)  http://127.0.0.1:$Port/" -ForegroundColor Green
  } catch {
    Write-Host "  [无应答] $($_.Exception.Message)" -ForegroundColor Red
  }
}

function Invoke-InstallTask {
  if (-not (Test-Path $TaskVbs)) { throw "找不到任务入口脚本：$TaskVbs" }
  if (-not (Test-Path $StartVbs)) { throw "找不到快捷方式入口脚本：$StartVbs" }
  if (-not (Test-Path $Exe)) { throw "找不到可执行文件：$Exe（先构建：go build -o bin/ipm-server.exe .）" }
  New-Item -ItemType Directory -Force -Path $RunDir | Out-Null

  # 任务动作里必须写绝对路径，并显式给 -WorkingDirectory：任务环境里没有 %~dp0，
  # 相对路径会落到 system32。
  # 不传 -Trigger 就是纯按需：Start-ScheduledTask / schtasks /run 唤得起，
  # 但不会开机自启——使用者要的是脱离 agent，不是自启。
  $action = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument ('"{0}"' -f $TaskVbs) -WorkingDirectory $RepoRoot
  $settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -MultipleInstances IgnoreNew
  Register-ScheduledTask -TaskName $TaskName -Action $action -Settings $settings -Force `
    -Description 'MailBasin 本地服务。由计划任务托管，所以重启 DSH 不会带走它。按需运行，无触发器。' | Out-Null
  Write-Host "已注册计划任务 $TaskName（按需、无触发器、不限执行时长、单实例）"

  # 桌面快捷方式指向 wscript.exe 跑启动脚本：wscript 是 GUI 子系统程序，
  # 不分配控制台，所以双击下去全程不会闪黑框。
  $desktop = [Environment]::GetFolderPath('Desktop')
  $lnkPath = Join-Path $desktop 'MailBasin 启动.lnk'
  $ws = New-Object -ComObject WScript.Shell
  $lnk = $ws.CreateShortcut($lnkPath)
  $lnk.TargetPath = 'wscript.exe'
  $lnk.Arguments = '"{0}"' -f $StartVbs
  $lnk.WorkingDirectory = $RepoRoot
  $lnk.Description = '静默启动 MailBasin 服务'
  $lnk.IconLocation = "$env:SystemRoot\System32\shell32.dll,13"
  $lnk.Save()
  Write-Host "已创建桌面快捷方式：$lnkPath"
}

function Invoke-UninstallTask {
  if (Test-TaskRegistered) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "已注销计划任务 $TaskName"
  } else {
    Write-Host "计划任务 $TaskName 本来就没注册。"
  }
  $lnkPath = Join-Path ([Environment]::GetFolderPath('Desktop')) 'MailBasin 启动.lnk'
  if (Test-Path $lnkPath) {
    Remove-Item -Force $lnkPath
    Write-Host "已删除桌面快捷方式：$lnkPath"
  }
}

switch ($Action) {
  'start'          { Invoke-Start }
  'stop'           { Invoke-Stop }
  'restart'        { Invoke-Stop; Invoke-Start }
  'status'         { Invoke-Status }
  'install-task'   { Invoke-InstallTask }
  'uninstall-task' { Invoke-UninstallTask }
}
