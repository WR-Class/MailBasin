<#
.SYNOPSIS
  MailBasin 一键更新：从仓库取到目标版本 → 本机构建 → 替换可执行文件 → 重启 → 验证 → 失败回滚。

.DESCRIPTION
  为什么这段逻辑不能放进服务自己：更新过程中要停掉、替换、再启动的正是服务本身。
  一个进程不能覆盖自己的镜像，也不该在自己被换掉的中途当裁判。所以这一段跑在服务
  之外，由计划任务 MailBasin-Update 承载（任务由计划任务服务托管，独立于 MailBasin
  任务，所以停掉服务不会连带停掉更新器）。

  更新来源是源码而不是二进制产物：这个仓库没有 tag、没有 release，只有 main；而构建
  好的前端产物（internal/webui/dist）本身是提交进仓库的，所以 git 取到源码就同时拿到了
  前端，构建只需要 git + go，不需要 Node/npm。npm 构建是整条链里最容易抽风的一步，
  能不开就不开。

  硬条件（缺一条都不动手）：
    * 工作树有「已跟踪文件」的改动 -> 拒绝。git reset --hard 会毁掉它们，而这是使用者的
      东西，不是更新器可以替他决定的东西。未跟踪的文件不在拒绝范围内：reset --hard 本来
      就不会删它们（只有 git clean 会，这里不跑 git clean）。
    * git / go 找不到 -> 拒绝，并说清找了哪些路径。
    * 可执行文件不存在 -> 拒绝。
    * 备份没做出来 -> 拒绝。宁可不更新，也不能在没有退路的情况下覆盖正在跑的程序。
    * 已经有一个更新在跑 -> 拒绝（任务本身设了 IgnoreNew，这里是第二道）。

  失败时的行为，逐段：
    * 取源码/构建阶段失败：把源码指针退回原提交、删掉半成品。正在跑的程序从未被动过。
    * 换上去之后起不来，或自报的提交对不上目标：换回旧程序、源码指针退回原提交、重启，
      再验证一次「旧版本真的活着」，然后如实报告——包括回滚本身失败的情况。

.PARAMETER Target
  更新到哪个版本，默认 origin/main。

.PARAMETER Port
  服务端口，默认 8788（和 server.ps1 一致）。

.PARAMETER VerifySeconds
  换上去之后等新版本应答的最长秒数，默认 60。

.PARAMETER InstallTask
  注册计划任务 MailBasin-Update 后退出。新机器上跑一次。

.PARAMETER UninstallTask
  注销计划任务 MailBasin-Update 后退出。

.PARAMETER SimulateVerifyFailure
  仅用于演练：装配完成后强制判定验证失败，用来真实地走一遍回滚。
  真实故障（构建坏了、新版本起不来）很难按需制造，而回滚是这套东西唯一值得信任的部分，
  所以它必须能被反复演练。这个开关只会让「验证」这一步失败，不会破坏别的东西。

.EXAMPLE
  scripts\update.ps1 -InstallTask
.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\update.ps1
#>
[CmdletBinding()]
param(
  [string]$Target = 'origin/main',
  [int]$Port = 8788,
  [int]$VerifySeconds = 60,
  [switch]$InstallTask,
  [switch]$UninstallTask,
  [switch]$SimulateVerifyFailure
)

$ErrorActionPreference = 'Stop'

# 仓库根：本脚本在 <root>\scripts\ 下。这三个路径和 server.ps1 保持一致，
# 不另立一套——bin/ 与 data/ 都在 .gitignore 里，所以更新器写的东西不会污染仓库。
$RepoRoot   = Split-Path -Parent $PSScriptRoot
$Exe        = Join-Path $RepoRoot 'bin\ipm-server.exe'
$NewExe     = Join-Path $RepoRoot 'bin\ipm-server.exe.new'
$OldExe     = Join-Path $RepoRoot 'bin\ipm-server.exe.old'
$BackupDir  = Join-Path $RepoRoot 'bin\backup'
$RunDir     = Join-Path $RepoRoot 'data\run'
$StatusFile = Join-Path $RunDir 'update-status.json'
$UpdateLog  = Join-Path $RunDir 'update.log'
$BuildLog   = Join-Path $RunDir 'update-build.log'
$ServerCmd  = Join-Path $PSScriptRoot 'server.cmd'
$TaskName   = 'MailBasin-Update'
$TaskVbs    = Join-Path $PSScriptRoot 'update-run-hidden.vbs'

New-Item -ItemType Directory -Force -Path $RunDir, $BackupDir | Out-Null

# —— 状态文件：界面靠它显示进度。服务会在更新中途重启，所以状态必须落在磁盘上，
#    不能只留在内存里。 ——
$script:FromCommit = ''
$script:ToCommit   = ''
$script:StartedAt  = (Get-Date).ToString('o')
$script:FinishedAt = ''
$script:FailureReason      = ''
$script:Swapped    = $false   # 是否已经动过正在跑的那个可执行文件
$script:TreeMoved  = $false   # 是否已经动过源码指针
$script:RolledBack = $false

function Write-Log {
  param([string]$Message)
  $line = "[{0}] {1}" -f (Get-Date).ToString('yyyy-MM-dd HH:mm:ss'), $Message
  Add-Content -Path $UpdateLog -Value $line -Encoding UTF8
}

# 不用 Set-Content -Encoding UTF8：PowerShell 5.1 会写 BOM，而带 BOM 的 JSON
# 喂给 Go 的 json 解码器会直接失败。所以显式写无 BOM 的 UTF8。
function Write-Status {
  param([string]$Step, [string]$Message, [string]$State = 'running')
  if ($State -ne 'running') { $script:FinishedAt = (Get-Date).ToString('o') }
  $body = [ordered]@{
    running     = ($State -eq 'running')
    state       = $State                       # running / done / failed / rolled-back
    step        = $Step
    message     = $Message
    pid         = $PID
    from_commit = $script:FromCommit
    to_commit   = $script:ToCommit
    started_at  = $script:StartedAt
    updated_at  = (Get-Date).ToString('o')
    finished_at = $script:FinishedAt
    error       = $script:FailureReason
    rolled_back = $script:RolledBack
    # 只有真的存在那个备份文件才报路径。否则拒绝更新时会出现 ipm-server-.exe
    # 这种名字（提交号是空的），看着像有个备份可用，其实什么都没做。
    exe_backup  = $(if ($script:BackupExe -and (Test-Path $script:BackupExe)) { $script:BackupExe } else { '' })
    log_path    = $UpdateLog
  }
  $json = $body | ConvertTo-Json -Depth 4
  [System.IO.File]::WriteAllText($StatusFile, $json, (New-Object System.Text.UTF8Encoding($false)))
  Write-Log ("{0} [{1}] {2}" -f $State, $Step, $Message)
}

function Resolve-Tool {
  param([string]$Name, [string]$FallbackPath)
  $cmd = Get-Command $Name -ErrorAction SilentlyContinue
  if ($cmd) { return $cmd.Source }
  if ($FallbackPath -and (Test-Path $FallbackPath)) { return $FallbackPath }
  return $null
}

$Git = Resolve-Tool -Name 'git' -FallbackPath 'C:\Program Files\Git\cmd\git.exe'
$Go  = Resolve-Tool -Name 'go'  -FallbackPath 'C:\Program Files\Go\bin\go.exe'

# 健康探测走 curl.exe：本机的系统代理开着，PowerShell 5.1 的 Invoke-WebRequest 会把它
# 带上，探测本机端口反而容易出问题；curl.exe 在 Windows 10+ 是自带的，行为稳定。
$Curl = Resolve-Tool -Name 'curl.exe' -FallbackPath (Join-Path $env:SystemRoot 'System32\curl.exe')

function Invoke-Native {
  param([string]$FilePath, [string[]]$Arguments)
  # 为什么要临时把 ErrorActionPreference 降回 Continue：PowerShell 会把原生命令写到 stderr
  # 的每一行包成 ErrorRecord，而本脚本开着 Stop——于是像 git fetch 这种
  # 「正常地把进度写到 stderr」的命令会被当成抛异常。这个坑是在演练里撞出来的：
  # fetch 明明成功，报错信息里却只有一行 "From https://…"。
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $output = & $FilePath @Arguments 2>&1
    $code = $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $previous
  }
  return [pscustomobject]@{ Code = $code; Output = @($output | ForEach-Object { "$_" }) }
}

function Invoke-ServerCmd {
  param([string]$Verb)
  Write-Log ("server.cmd {0}" -f $Verb)
  $result = Invoke-Native -FilePath $ServerCmd -Arguments @($Verb)
  $result.Output | ForEach-Object { Write-Log ("  " + $_) }
  return $result.Code
}

function Get-HealthCommit {
  if (-not $Curl) { return $null }
  $raw = & $Curl -s --max-time 5 ("http://127.0.0.1:{0}/api/health" -f $Port)
  if (-not $raw) { return $null }
  try { $json = $raw | ConvertFrom-Json } catch { return $null }
  if (-not $json.success) { return $null }
  return [string]$json.data.commit
}

function Wait-HealthCommit {
  param([int]$Seconds)
  $deadline = (Get-Date).AddSeconds($Seconds)
  while ((Get-Date) -lt $deadline) {
    $commit = Get-HealthCommit
    if ($commit) { return $commit }
    Start-Sleep -Milliseconds 700
  }
  return $null
}

function Invoke-Git {
  param([string[]]$Arguments)
  $result = Invoke-Native -FilePath $Git -Arguments (@('-C', $RepoRoot) + $Arguments)
  if ($result.Code -ne 0) {
    throw ("git {0} 失败（退出码 {1}）：{2}" -f ($Arguments -join ' '), $result.Code, ($result.Output -join ' / '))
  }
  return $result.Output
}

function Get-TrackedChanges {
  # 只看已跟踪文件的改动：那正是 git reset --hard 会毁掉的东西。
  $out = & $Git -C $RepoRoot status --porcelain --untracked-files=no
  return @($out | Where-Object { $_ -and $_.Trim() })
}

function Invoke-Rollback {
  param([string]$Reason)
  # 返回四种结局，让收尾能如实说话：
  #   noop        前置条件就没过，什么都没动过——这不是回滚，是拒绝
  #   untouched   做了些事（比如源码指针动过）但从未碰过正在跑的程序——没有东西被装上去
  #   restored    真的换过，并且已经确认旧版本在运行
  #   unconfirmed 换了但没能确认恢复——必须大声报告
  if (-not $script:Swapped -and -not $script:TreeMoved) {
    Write-Log ("不需要回滚：还没动过源码或可执行文件（{0}）" -f $Reason)
    return 'noop'
  }
  # 服务只在「真的被动过」的时候才停/起。构建失败就是没被动过的典型：可执行文件没换、
  # 服务也没停过。这时候去停一次再起一次，等于白造一次停机，而且什么都没修。
  $serviceTouched = $script:Swapped

  if ($serviceTouched) {
    [void](Invoke-ServerCmd 'stop')

    # 优先用换下来的那个（一次同目录改名，内容必然一致）；退而用按版本存的快照。
    if (Test-Path $OldExe) {
      Move-Item -Force $OldExe $Exe
      Write-Log "已用 ipm-server.exe.old 还原可执行文件"
    } elseif (Test-Path $script:BackupExe) {
      Copy-Item -Force $script:BackupExe $Exe
      Write-Log ("已用备份还原可执行文件：{0}" -f $script:BackupExe)
    } else {
      # 走到这里说明前置条件没守住（没有备份就换了）。必须大声说出来，不能假装回滚成功。
      throw "没有可用的旧版本可执行文件，无法回滚。原提交是 $($script:FromCommit)"
    }
    $script:Swapped = $false
  } else {
    Write-Log '可执行文件从未被替换、服务也从未被停过：不动它，只把源码指针退回去'
  }

  if ($script:TreeMoved) {
    [void](Invoke-Git @('reset', '--hard', $script:FromCommit))
    $script:TreeMoved = $false
    Write-Log ("源码指针已退回 {0}" -f $script:FromCommit)
  }

  if ($serviceTouched) {
    [void](Invoke-ServerCmd 'start')
  }
  $live = Wait-HealthCommit -Seconds $VerifySeconds

  if (-not $serviceTouched) {
    # 从头到尾没碰过服务，它跑的还是原来那个程序。唯一可能的例外是它本来就和
    # 源码指针不一致（更新之前就是脱节的），那不是这次更新造成的，
    # 所以不该说成「回滚未确认」——那会把一个并未发生的危险说成发生了。
    if (-not $live) {
      Write-Log '没有动过服务，但它没有应答——需要人工看一眼'
      return 'unconfirmed'
    }
    Write-Log ("没有装上任何东西，服务全程没停过，仍在运行：{0}" -f $live)
    $script:RolledBack = $true
    return 'untouched'
  }

  if ($live -eq $script:FromCommit) {
    Write-Log ("回滚完成，旧版本已在运行：{0}" -f $live)
    $script:RolledBack = $true
    return 'restored'
  }
  Write-Log ("回滚后旧版本未能确认在运行：期望 {0}，实际 {1}" -f $script:FromCommit, $live)
  return 'unconfirmed'
}

function Invoke-UpdateTask {
  if (-not $Git) { throw "找不到 git。找过 PATH，也找过 C:\Program Files\Git\cmd\git.exe" }
  if (-not $Go)  { throw "找不到 go。找过 PATH，也找过 C:\Program Files\Go\bin\go.exe" }
  if (-not (Test-Path $Exe)) { throw "找不到可执行文件：$Exe" }
  if (-not $Curl) { throw "找不到 curl.exe，无法做健康验证" }

  # 锁检查必须先于写自己的状态。否则会先写下 running=true、pid=自己，紧接着把它读回来发现
  # 「已经有一个更新在跑」——那个正是自己。这个自锁是在演练里被发现的。
  if (Test-Path $StatusFile) {
    # 显式按 UTF-8 读：这个文件是无 BOM 的 UTF-8，而 PowerShell 5.1 的 Get-Content
    # 对无 BOM 文件会按 ANSI 代码页解码，中文会变成乱码（这里只读 ASCII 字段，
    # 但解码方式不该靠运气）。
    try {
      $raw = [System.IO.File]::ReadAllText($StatusFile, [System.Text.Encoding]::UTF8)
      $previous = $raw | ConvertFrom-Json
    } catch { $previous = $null }
    if ($previous -and $previous.running -and ([int]$previous.pid -ne $PID)) {
      $alive = Get-Process -Id ([int]$previous.pid) -ErrorAction SilentlyContinue
      if ($alive) { throw ("已经有一个更新在跑（PID {0}，步骤 {1}）" -f $previous.pid, $previous.step) }
    }
  }

  Write-Status -Step 'precheck' -Message '正在检查前置条件'

  $changes = Get-TrackedChanges
  if ($changes.Count -gt 0) {
    throw ("工作树有未提交的改动，更新会毁掉它们，所以先停在这里。请先提交或 stash，再点更新。涉及：" + ($changes -join ' ; '))
  }

  $script:FromCommit = ((Invoke-Git @('rev-parse', 'HEAD')) | Select-Object -First 1).Trim()
  Write-Log ("当前提交 {0}，目标 {1}" -f $script:FromCommit, $Target)

  Write-Status -Step 'fetch' -Message ("正在读取仓库（{0}）" -f $Target)
  [void](Invoke-Git @('fetch', 'origin', 'main', '--prune'))
  $script:ToCommit = ((Invoke-Git @('rev-parse', ("{0}^{{commit}}" -f $Target))) | Select-Object -First 1).Trim()
  Write-Log ("目标提交 {0}" -f $script:ToCommit)

  # 源码在目标提交上，还要看「正在运行的是不是它」。工作树和可执行文件是可以脱节的——
  # 比如有人手工 reset 了源码却没重新构建。这时说「已经是最新版本」就是假话：
  # 使用者要的是让运行中的程序对齐 origin/main，而不是让工作树对齐。
  # 这个盲点是演练里撞出来的：把树 reset 到旧提交但不重编，更新器就宣布「已是最新」什么都不做。
  if ($script:ToCommit -eq $script:FromCommit) {
    $runningCommit = Get-HealthCommit
    if ($runningCommit -eq $script:ToCommit) {
      Write-Status -Step 'done' -State 'done' -Message ("已经是最新版本（{0}），且正在运行的就是它，没有做任何改动" -f $script:FromCommit.Substring(0, 12))
      return
    }
    Write-Log ("源码已在 {0}，但正在运行的是 {1}：需要重新构建并装配" -f $script:FromCommit, $runningCommit)
    Write-Status -Step 'checkout' -Message ("源码已是最新，但正在运行的不是它（{0}），重新构建并装配" -f $runningCommit)
  }

  # 备份：这是所有后续步骤的退路，先做出来并逐字节确认，再往下走。
  $script:BackupExe = Join-Path $BackupDir ("ipm-server-{0}.exe" -f $script:FromCommit)
  Write-Status -Step 'backup' -Message '正在备份当前可执行文件'
  Copy-Item -Force $Exe $script:BackupExe
  $srcHash = (Get-FileHash -Algorithm SHA256 $Exe).Hash
  $bakHash = (Get-FileHash -Algorithm SHA256 $script:BackupExe).Hash
  if ($srcHash -ne $bakHash) { throw "备份校验不一致，拒绝更新。当前 $srcHash，备份 $bakHash" }
  Write-Log ("备份已确认：{0}（SHA256 {1}）" -f $script:BackupExe, $srcHash.Substring(0, 16))

  # —— 取源码 + 构建。这一段完全不碰正在跑的程序；失败只需把源码指针退回去。 ——
  Write-Status -Step 'checkout' -Message ("正在切换到 {0}" -f $script:ToCommit.Substring(0, 12))
  [void](Invoke-Git @('reset', '--hard', $script:ToCommit))
  $script:TreeMoved = $true

  Write-Status -Step 'build' -Message '正在本机构建（前端产物在仓库里，所以不需要 Node）'
  $buildArgs = @('build', '-trimpath', '-ldflags', '-s -w', '-o', $NewExe, '.')
  $build = Invoke-Native -FilePath $Go -Arguments $buildArgs
  [System.IO.File]::WriteAllText($BuildLog, ($build.Output -join "`r`n"), (New-Object System.Text.UTF8Encoding($false)))
  if ($build.Code -ne 0) {
    Remove-Item -Force $NewExe -ErrorAction SilentlyContinue
    throw ("构建失败（退出码 {0}）。构建日志：{1}" -f $build.Code, $BuildLog)
  }
  if (-not (Test-Path $NewExe)) { throw ("构建没有产出可执行文件，日志：{0}" -f $BuildLog) }
  Write-Log ("构建完成：{0}" -f $NewExe)

  # —— 装配。从这里开始动正在跑的程序，任何失败都要回滚。 ——
  Write-Status -Step 'stop' -Message '正在停止服务'
  [void](Invoke-ServerCmd 'stop')

  Write-Status -Step 'swap' -Message '正在替换可执行文件'
  Move-Item -Force $Exe $OldExe
  $script:Swapped = $true
  Move-Item -Force $NewExe $Exe

  Write-Status -Step 'start' -Message '正在启动新版本'
  [void](Invoke-ServerCmd 'start')

  Write-Status -Step 'verify' -Message ("正在确认新版本真的起来了（期望提交 {0}）" -f $script:ToCommit.Substring(0, 12))
  if ($SimulateVerifyFailure) {
    Write-Log '演练开关已打开：强制判定验证失败，用来走一遍回滚'
    throw '演练：SimulateVerifyFailure'
  }
  $live = Wait-HealthCommit -Seconds $VerifySeconds
  if ($live -ne $script:ToCommit) {
    throw ("新版本没能确认在运行：期望 {0}，实际 {1}" -f $script:ToCommit, $live)
  }

  # 装配成功了，换下来的那个就没用了：它只是本次运行里的快捷退路，留着等于每次
  # 更新都白占 13.8 MB。按版本存的备份仍在 bin\backup 里，回滚不依赖它。
  Remove-Item -Force $OldExe -ErrorAction SilentlyContinue

  Write-Log ("更新完成：{0} -> {1}" -f $script:FromCommit, $live)
  Write-Status -Step 'done' -State 'done' -Message ("已更新到 {0}" -f $live.Substring(0, 12))
}

function Invoke-InstallTask {
  if (-not (Test-Path $TaskVbs)) { throw "找不到任务入口脚本：$TaskVbs" }
  # 与 MailBasin 任务同一套：按需、无触发器、不限执行时长、单实例。
  # 不传 -Trigger 就是纯按需——Start-ScheduledTask 唤得起，但不会开机自启。
  # 两个任务互相独立，所以更新器停掉服务不会把自己也停掉。
  $action = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument ('"{0}"' -f $TaskVbs) -WorkingDirectory $RepoRoot
  $settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -MultipleInstances IgnoreNew
  Register-ScheduledTask -TaskName $TaskName -Action $action -Settings $settings -Force `
    -Description 'MailBasin 一键更新。由计划任务托管，所以它能在停掉服务、替换可执行文件的过程中活着。按需运行，无触发器。' | Out-Null
  Write-Host "已注册计划任务 $TaskName（按需、无触发器、不限执行时长、单实例）"
}

function Invoke-UninstallTask {
  if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "已注销计划任务 $TaskName"
  } else {
    Write-Host "计划任务 $TaskName 本来就没注册。"
  }
}

if ($InstallTask)    { Invoke-InstallTask; return }
if ($UninstallTask)  { Invoke-UninstallTask; return }

try {
  Invoke-UpdateTask
} catch {
  $script:FailureReason = $_.Exception.Message
  Write-Log ("失败：{0}" -f $script:FailureReason)
  $outcome = 'unconfirmed'
  try {
    $outcome = Invoke-Rollback -Reason $script:FailureReason
  } catch {
    Write-Log ("回滚本身失败：{0}" -f $_.Exception.Message)
    $script:FailureReason = $script:FailureReason + " ；回滚也失败：" + $_.Exception.Message
  }
  switch ($outcome) {
    'noop' {
      # 前置条件没过，一个字节都没动过。说「已回滚」是不诚实的。
      Write-Status -Step 'failed' -State 'refused' -Message ("已拒绝，没有做任何改动：{0}" -f $script:FailureReason)
    }
    'untouched' {
      # 从未装上去任何东西，服务也没停过。说「已回滚」也是不准确的：
      # 没有东西需要回滚，原来的程序一直在跑。
      Write-Status -Step 'failed' -State 'not-installed' -Message ("没有装上任何东西，服务全程没停过：{0}" -f $script:FailureReason)
    }
    'restored' {
      Write-Status -Step 'failed' -State 'rolled-back' -Message ("更新失败，已回滚到原版本：{0}" -f $script:FailureReason)
    }
    default {
      Write-Status -Step 'failed' -State 'failed' -Message ("更新失败，且未能确认回滚成功：{0}" -f $script:FailureReason)
    }
  }
  exit 1
}
