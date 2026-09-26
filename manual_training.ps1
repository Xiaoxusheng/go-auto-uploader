Write-Host ""
Write-Host "============================================" -ForegroundColor Green
Write-Host "  高光姿态训练（手动触发）"
Write-Host "  ① 新片入池：抽帧 + 姿态推理 + 自动预标"
Write-Host "  ② 重定标：结果更新到控制台「姿态训练」页"
Write-Host "============================================" -ForegroundColor Green
Write-Host ""

$free = [math]::Round((Get-PSDrive D).Free/1GB, 1)
Write-Host "D 盘剩余 $free GB"
if ($free -lt 5) {
    Write-Host "⚠️ 磁盘不足 5GB，入池抽帧可能失败，建议先清理磁盘！" -ForegroundColor Red
}
Write-Host ""

Set-Location "D:\upload\_diag\train"
$log = "D:\upload\_diag\train\autotrain_hourly.log"
"=== $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') 手动训练轮次" | Out-File $log -Append -Encoding utf8

Write-Host "=== [1/2] 新片入池（无新片时几秒即结束，属正常）..." -ForegroundColor Cyan
& "D:\upload\_diag\train\hleval.exe" review-ingest `
    -downloads "D:/upload/downloads" `
    -frames "D:/upload/_diag/train/_pose_pilot/frames" `
    -config "D:/upload/_diag/train/_pose_pilot/clips_config.json" `
    -pose-out "D:/upload/_diag/train/pose_features_go.json" `
    -dll "D:/upload/onnxruntime.dll" `
    -model "D:/upload/yolov8n-pose.onnx" `
    -ffmpeg "D:/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe" 2>&1 | Tee-Object -FilePath $log -Append
if ($LASTEXITCODE -ne 0) {
    Write-Host "❌ 入池失败，请把上面的报错截图给管理员" -ForegroundColor Red
    Read-Host "按回车退出"
    exit 1
}

Write-Host ""
Write-Host "=== [2/2] 金标重定标..." -ForegroundColor Cyan
node "D:\upload\_diag\train\autogold_sweep.js" 2>&1 | Tee-Object -FilePath $log -Append
if ($LASTEXITCODE -ne 0) {
    Write-Host "❌ 重定标失败，请把上面的报错截图给管理员" -ForegroundColor Red
    Read-Host "按回车退出"
    exit 1
}

Write-Host ""
Write-Host "✅ 训练完成！打开控制台 http://127.0.0.1:8080 的「姿态训练」页查看最新数据" -ForegroundColor Green
Read-Host "按回车退出"
