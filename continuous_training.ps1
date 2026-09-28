# 连续训练守护：循环执行「新片入池 → 金标重定标」，直到关窗或磁盘护栏触发。
# 与 手动训练.bat 用同一套命令；日志追加到 autotrain_hourly.log（控制台「姿态训练」页实时解析）。
#
# 2026-09-27 改事件驱动：轮询间隔 180s → 900s；autogold-sweep 只在金标/特征指纹变化时执行。
# 依据：输入不变时 sweep 输出必然不变（实测第 183/184 轮 TOP8 逐字相同），
# 每 3 分钟空跑一轮纯属烧 CPU 与抽帧。入池新增片会改 pose 特征文件 → 指纹变化 → 自动触发 sweep。
$ErrorActionPreference = 'Continue'
$title = 'Pose Continuous Training'
$host.UI.RawUI.WindowTitle = $title

# 单实例护栏：已有连续训练在跑就退出，避免两个循环抢同一批片
$dup = Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" |
    Where-Object { $_.CommandLine -match 'continuous_training\.ps1' -and $_.ProcessId -ne $PID }
if ($dup) {
    Write-Host "已有连续训练在运行（PID $($dup.ProcessId)），本实例退出。" -ForegroundColor Yellow
    Start-Sleep 5
    exit 0
}

$log = 'D:\upload\_diag\train\autotrain_hourly.log'
$hleval = 'D:\upload\_diag\train\hleval.exe'
$gold = 'D:\upload\_diag\train\_pose_pilot\gold_review.json'
$pose = 'D:\upload\_diag\train\pose_features_go.json'
$fpFile = 'D:\upload\_diag\train\_sweep_fingerprint.txt'
$intervalSec = 900

# 重定标输入指纹：金标 + 每秒姿态特征的文件长度与 mtime。
# 两者都不变 → sweep 结果必然与上轮相同，直接跳过整轮网格搜索。
function Get-SweepFingerprint {
    $parts = @()
    foreach ($p in @($gold, $pose)) {
        if (Test-Path $p) {
            $f = Get-Item $p
            $parts += "$($f.Length):$($f.LastWriteTimeUtc.Ticks)"
        } else {
            $parts += 'missing'
        }
    }
    return ($parts -join '|')
}
Write-Host '============================================' -ForegroundColor Green
Write-Host '  连续训练守护已启动：入池 → 重定标（输入变化时才重算）'
Write-Host "  轮询间隔 $intervalSec 秒，关闭本窗口即停止训练"
Write-Host '============================================' -ForegroundColor Green

$round = 0
while ($true) {
    $round++
    $free = [math]::Round((Get-PSDrive D).Free / 1GB, 1)
    $head = "=== $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') 连续训练第 $round 轮（磁盘 $free GB）"
    Write-Host $head -ForegroundColor Cyan
    $head | Out-File $log -Append -Encoding utf8

    # [0] 磁盘自动治理：删「训练数据已用完」的源片（已入池=1fps 帧+每秒特征均已落盘，
    # 源片不再被任何环节引用）。保护集（冻结 v2/探针源）与 1 小时内新写文件一律跳过，
    # 全部删除动作追加审计日志 _deleted_ingested_sources.log。
    # 先清后判：低磁盘时先腾空间，下面的入池护栏用清理后的余量。
    $cleanArgs = 'cleanup-sources' +
        ' -downloads "D:/upload/downloads"' +
        ' -config "D:/upload/_diag/train/_pose_pilot/clips_config.json"' +
        ' -frames "D:/upload/_diag/train/_pose_pilot/frames"' +
        ' -protect-dirs "D:/upload/_diag/train/freeze_v2_sources,D:/upload/_diag/train/audio_probe/sources"' +
        ' -log "D:/upload/_diag/train/_deleted_ingested_sources.log"'
    cmd /c "`"$hleval`" $cleanArgs >> `"$log`" 2>&1"
    $free = [math]::Round((Get-PSDrive D).Free / 1GB, 1)

    if ($free -ge 10) {
        Write-Host '[1/2] 新片入池（抽帧 + 姿态推理 + 预标）...' -ForegroundColor Cyan
        # 走 cmd 重定向（字节级、实时落盘）：PowerShell 管道 + 逐行 Out-File 会
        # 缓冲输出（日志不更新 -> 控制台误判“待机”）并按 GBK 解码 UTF-8（中文乱码）。
        $ingestArgs = 'review-ingest' +
            ' -downloads "D:/upload/downloads"' +
            ' -frames "D:/upload/_diag/train/_pose_pilot/frames"' +
            ' -config "D:/upload/_diag/train/_pose_pilot/clips_config.json"' +
            ' -pose-out "D:/upload/_diag/train/pose_features_go.json"' +
            ' -dll "D:/upload/onnxruntime.dll"' +
            ' -model "D:/upload/yolov8n-pose.onnx"' +
            ' -ffmpeg "D:/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe"'
        cmd /c "`"$hleval`" $ingestArgs >> `"$log`" 2>&1"
        if ($LASTEXITCODE -ne 0) {
            Write-Host "入池退出码 $LASTEXITCODE（继续下一轮）" -ForegroundColor Red
            "review-ingest 退出码 $LASTEXITCODE" | Out-File $log -Append -Encoding utf8
        }
    } else {
        Write-Host "磁盘 ${free}GB < 10GB，跳过入池（只重定标）" -ForegroundColor Yellow
        "磁盘 ${free}GB < 10GB，跳过入池，仅重定标" | Out-File $log -Append -Encoding utf8
    }

    # 事件驱动：金标/特征指纹未变则跳过（输入相同 → 输出必然相同）
    $fp = Get-SweepFingerprint
    $lastFp = ''
    if (Test-Path $fpFile) { $lastFp = (Get-Content $fpFile -Raw -Encoding utf8).Trim() }
    if ($fp -eq $lastFp) {
        Write-Host '[2/2] 输入未变，跳过重定标（结果与上轮相同）' -ForegroundColor DarkGray
        '=== 输入未变，跳过重定标' | Out-File $log -Append -Encoding utf8
    } else {
        Write-Host '[2/2] 金标重定标...' -ForegroundColor Cyan
        cmd /c "`"$hleval`" autogold-sweep >> `"$log`" 2>&1"
        if ($LASTEXITCODE -eq 0) {
            # 只有成功才记指纹；失败留待下轮重试
            $fp | Out-File $fpFile -Encoding utf8 -NoNewline
        } else {
            "autogold-sweep 退出码 $LASTEXITCODE（下轮重试）" | Out-File $log -Append -Encoding utf8
        }
    }
    '=== 连续训练轮次完成' | Out-File $log -Append -Encoding utf8

    Start-Sleep -Seconds $intervalSec
}
