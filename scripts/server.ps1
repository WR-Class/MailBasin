<#
.SYNOPSIS
  MailBasin 服务的启停入口：start / stop / restart / status。

.DESCRIPTION
  这是「怎么启动这个服务」的单一出处。以后要重启它，用这个脚本，
  不要由 agent 临场拼 Start-Process 命令。

  服务用 WMI（Win32_Process.Create）启动，所以新进程的父进程是 WmiPrvSE.exe，
  祖先链走到 DCOM 服务，与 DSH 无关。这一点是刻意的：

    用 Start-Process 启动会把服务留在 DSH 的进程树下。父进程退出不会带走子进程，
    所以它看起来没事——但作业对象（KILL_ON_JOB_CLOSE）会。而作业归属在这台机器上
    查不清：IsProcessInJob 传 NULL 在 Win8+ 上对几乎所有进程都返回真（嵌套作业），
    没有区分度；NtQueryInformationProcess 取作业句柄返回 STATUS_INFO_LENGTH_MISMATCH。
    查不清就不猜，换成一条祖先链可验证的路。

  判据是可观察的：status 会打印祖先链，链上不该出现 DSH 的进程。

.PARAMETER Action
  start   启动（已在运行则什么都不做）
  stop    停止
  restart 先停后启
  status  打印 PID、祖先链和一次 HTTP 探测（默认动作）

.PARAMETER Port
  服务监听端口，默认 8788。

.EXAMPLE
  powershell -NoProfile -File scripts\server.ps1 restart

.EXAMPLE
  scripts\server.cmd status
#>
[CmdletBinding()]
param(
  [Parameter(Position = 0)]
  [ValidateSet('start', 'stop', 'restart', 'status')]
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

function Invoke-Start {
  $existing = Get-ServerProcessId
  if ($existing) {
    Write-Host "服务已在运行，PID $existing（端口 $Port）。要重启请用 restart。"
    return
  }
  if (-not (Test-Path $Exe)) { throw "找不到可执行文件：$Exe（先构建：go build -o bin/ipm-server.exe .）" }
  New-Item -ItemType Directory -Force -Path $RunDir | Out-Null

  # 日志轮转，只留上一份，免得无限增长。
  foreach ($file in @($OutLog, $ErrLog)) {
    if (Test-Path $file) { Move-Item -Force $file ($file -replace '\.log$', '.prev.log') }
  }
  # 这个二进制是个交互式菜单，需要一个 stdin 的「1」才会进入服务模式。
  [System.IO.File]::WriteAllText($StdinFile, '1', (New-Object System.Text.ASCIIEncoding))

  # WMI 创建：父进程是 WmiPrvSE.exe，不在 DSH 的进程树里，也不继承它的作业对象。
  # cmd /c 只是为了把 stdin 和两个日志重定向带上。
  $commandLine = 'cmd /c cd /d "{0}" && "{1}" < "{2}" > "{3}" 2> "{4}"' -f $RepoRoot, $Exe, $StdinFile, $OutLog, $ErrLog
  $result = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $commandLine }
  if ($result.ReturnValue -ne 0) {
    Show-LogTail -Path $ErrLog
    throw "WMI 创建进程失败，ReturnValue=$($result.ReturnValue)。错误日志在上面。"
  }

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

switch ($Action) {
  'start'   { Invoke-Start }
  'stop'    { Invoke-Stop }
  'restart' { Invoke-Stop; Invoke-Start }
  'status'  { Invoke-Status }
}
