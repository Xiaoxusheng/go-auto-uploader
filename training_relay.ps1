# 接力守护：等当前手动训练（hleval + autogold_sweep）跑完后，自动启动连续训练守护
while ($true) {
    $hleval = Get-Process hleval -ErrorAction SilentlyContinue
    $sweep = Get-CimInstance Win32_Process -Filter "Name='node.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -match 'autogold_sweep' }
    if (-not $hleval -and -not $sweep) { break }
    Start-Sleep -Seconds 20
}
Start-Sleep -Seconds 15
Start-Process powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','D:\upload\continuous_training.ps1' -WindowStyle Hidden
