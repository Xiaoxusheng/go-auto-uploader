# 连续训练守护：循环执行「新片入池 → 金标重定标」，直到关窗或磁盘护栏触发。
# 与 手动训练.bat 用同一套命令；日志追加到 autotrain_hourly.log（控制台「姿态训练」页实时解析）。
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
Write-Host '============================================' -ForegroundColor Green
Write-Host '  连续训练守护已启动：入池 → 重定标，每 3 分钟一轮'
Write-Host '  关闭本窗口即停止训练'
Write-Host '============================================' -ForegroundColor Green

$round = 0
while ($true) {
    $round++
    $free = [math]::Round((Get-PSDrive D).Free / 1GB, 1)
    $head = "=== $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') 连续训练第 $round 轮（磁盘 $free GB）"
    Write-Host $head -ForegroundColor Cyan
    $head | Out-File $log -Append -Encoding utf8

    if ($free -ge 10) {
        Write-Host '[1/2] 新片入池（抽帧 + 姿态推理 + 预标）...' -ForegroundColor Cyan
        & $hleval review-ingest `
            -downloads 'D:/upload/downloads' `
            -frames 'D:/upload/_diag/train/_pose_pilot/frames' `
            -config 'D:/upload/_diag/train/_pose_pilot/clips_config.json' `
            -pose-out 'D:/upload/_diag/train/pose_features_go.json' `
            -dll 'D:/upload/onnxruntime.dll' `
            -model 'D:/upload/yolov8n-pose.onnx' `
            -ffmpeg 'D:/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe' 2>&1 |
            Tee-Object -FilePath $log -Append
        if ($LASTEXITCODE -ne 0) {
            Write-Host "入池退出码 $LASTEXITCODE（继续下一轮）" -ForegroundColor Red
            "review-ingest 退出码 $LASTEXITCODE" | Out-File $log -Append -Encoding utf8
        }
    } else {
        Write-Host "磁盘 ${free}GB < 10GB，跳过入池（只重定标）" -ForegroundColor Yellow
        "磁盘 ${free}GB < 10GB，跳过入池，仅重定标" | Out-File $log -Append -Encoding utf8
    }

    Write-Host '[2/2] 金标重定标...' -ForegroundColor Cyan
    node 'D:\upload\_diag\train\autogold_sweep.js' 2>&1 | Tee-Object -FilePath $log -Append
    '=== 连续训练轮次完成' | Out-File $log -Append -Encoding utf8

    Start-Sleep -Seconds 180
}
