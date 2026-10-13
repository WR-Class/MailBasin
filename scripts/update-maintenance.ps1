<#
.SYNOPSIS
  MailBasin 更新器的产物清理：给备份数量和日志大小各设一个上限。

.DESCRIPTION
  更新器每跑一次就留下只增不减的东西：bin\backup 里一个 13.8 MB 的按版本备份，
  以及 data\run 下两份追加写的日志。备份是回滚的退路，不能不留；日志是排查的依据，
  也不能不留——但两者都不该无限增长。

  这段逻辑单独成文件，不塞进 update.ps1：塞进去就只能靠「真的更新一次」来验证它，
  而清理规则恰恰是最该被单独断言的东西——它删的是回滚的退路。这里可以对着一个夹具
  直接跑，指定保留几个，看它到底留了哪几个。

  清理只删它自己认得的产物：
    * bin\backup\ipm-server-<提交号>.exe  按修改时间保留最新的 N 个
    * data\run\update.log / update-build.log  超过上限就只留最后若干行
    * bin\ipm-server.exe.new  构建中途留下的半成品（有就删）
  除此之外什么都不碰，尤其是 data\app.db —— 那是使用者的邮件库，不是本脚本的产物。

.PARAMETER KeepBackups
  保留最新的几个按版本备份，默认 3。

.PARAMETER LogMaxBytes
  日志超过这么多字节才截断，默认 524288（512 KB）。

.PARAMETER LogKeepLines
  截断时保留最后多少行，默认 200。留尾部而不是头部：最新的那几行才是人要看的。

.PARAMETER Root
  仓库根目录，默认取本脚本所在目录的上一级。

.PARAMETER DryRun
  只报告会删什么，不真删。

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\update-maintenance.ps1
.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\update-maintenance.ps1 -KeepBackups 2 -DryRun
#>
[CmdletBinding()]
param(
  [int]$KeepBackups = 3,
  [int]$LogMaxBytes = 524288,
  [int]$LogKeepLines = 200,
  [string]$Root = '',
  [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

if (-not $Root) { $Root = Split-Path -Parent $PSScriptRoot }
$BackupDir = Join-Path $Root 'bin\backup'
$RunDir    = Join-Path $Root 'data\run'
$NewExe    = Join-Path $Root 'bin\ipm-server.exe.new'

$removed = 0
$freed = 0

# —— 1. 按版本备份：保留最新的 N 个 ——
# 排序带上 Name 作为第二键：LastWriteTime 精度到秒，同一秒内做出来的两个备份若只按时间排，
# 谁留谁删就不确定了，而这里删的是回滚的退路，不能有不确定。
if (Test-Path $BackupDir) {
  $all = @(Get-ChildItem -Path $BackupDir -Filter 'ipm-server-*.exe' -File -ErrorAction SilentlyContinue |
             Sort-Object LastWriteTime, Name -Descending)
  $doomed = @($all | Select-Object -Skip $KeepBackups)
  Write-Host ("备份：现有 {0} 个，保留最新的 {1} 个，删除 {2} 个" -f $all.Count, [Math]::Min($KeepBackups, $all.Count), $doomed.Count)
  foreach ($file in $doomed) {
    $size = $file.Length
    $when = $file.LastWriteTime.ToString('yyyy-MM-dd HH:mm')
    if ($DryRun) {
      Write-Host ("  [预演] 会删除 {0}（{1:N1} MB，{2}）" -f $file.Name, ($size / 1MB), $when)
    } else {
      Remove-Item -LiteralPath $file.FullName -Force
      Write-Host ("  已删除 {0}（{1:N1} MB，{2}）" -f $file.Name, ($size / 1MB), $when)
    }
    $removed += 1
    $freed += $size
  }
  if ($doomed.Count -eq 0) {
    Write-Host ("  没有需要删的。保留：{0}" -f (($all | ForEach-Object { $_.Name }) -join ', '))
  }
} else {
  Write-Host ("备份目录不存在，跳过：{0}" -f $BackupDir)
}

# —— 2. 日志：超过上限就只留最后若干行 ——
foreach ($name in @('update.log', 'update-build.log')) {
  $path = Join-Path $RunDir $name
  if (-not (Test-Path $path)) { Write-Host ("日志不存在，跳过：{0}" -f $name); continue }
  $size = (Get-Item -LiteralPath $path).Length
  if ($size -le $LogMaxBytes) {
    Write-Host ("日志 {0}：{1:N0} 字节，未超过上限 {2:N0}，不动" -f $name, $size, $LogMaxBytes)
    continue
  }
  $lines = @(Get-Content -LiteralPath $path -Encoding UTF8)
  $kept = @($lines | Select-Object -Last $LogKeepLines)
  if ($DryRun) {
    Write-Host ("  [预演] {0}：{1:N0} 字节 / {2} 行 -> 只留最后 {3} 行" -f $name, $size, $lines.Count, $kept.Count)
    continue
  }
  $dropped = $lines.Count - $kept.Count
  $header = "（已截断：{0} 之前的 {1} 行被清理，保留最后 {2} 行）" -f (Get-Date).ToString('yyyy-MM-dd HH:mm:ss'), $dropped, $kept.Count
  # 显式写无 BOM 的 UTF8：PS 5.1 的 Set-Content -Encoding UTF8 会写 BOM，而更新器是用
  # Add-Content 往这个文件里追加的（按字节追加），混着 BOM 会让下次读取多出一个字符。
  [System.IO.File]::WriteAllLines($path, (@($header) + $kept), (New-Object System.Text.UTF8Encoding($false)))
  $newSize = (Get-Item -LiteralPath $path).Length
  Write-Host ("  已截断 {0}：{1:N0} 字节 -> {2:N0} 字节（丢掉 {3} 行，留最后 {4} 行）" -f $name, $size, $newSize, $dropped, $kept.Count)
  $freed += ($size - $newSize)
}

# —— 3. 构建半成品 ——
# 只在更新成功之后才会跑本脚本，所以此刻还躺着的 .new 一定是某次失败的残留。
if (Test-Path $NewExe) {
  $size = (Get-Item -LiteralPath $NewExe).Length
  if ($DryRun) {
    Write-Host ("  [预演] 会删除构建半成品 ipm-server.exe.new（{0:N1} MB）" -f ($size / 1MB))
  } else {
    Remove-Item -LiteralPath $NewExe -Force
    Write-Host ("  已删除构建半成品 ipm-server.exe.new（{0:N1} MB）" -f ($size / 1MB))
  }
  $removed += 1
  $freed += $size
} else {
  Write-Host '没有构建半成品残留'
}

if ($DryRun) {
  Write-Host ("预演结束：会删 {0} 个文件，可回收约 {1:N1} MB。什么都没改。" -f $removed, ($freed / 1MB))
} else {
  Write-Host ("清理结束：删了 {0} 个文件，回收约 {1:N1} MB" -f $removed, ($freed / 1MB))
}
