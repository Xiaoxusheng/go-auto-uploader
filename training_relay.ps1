# 接力守护：等当前手动训练（hleval 入池 + hleval autogold-sweep 重定标）跑完后，自动启动连续训练守护
# node 旧版 sweep 脚本兼容保留：匹配 autogold[_-]sweep 两种写法
while ($true) {
    $hleval = Get-Process hleval -ErrorAction SilentlyContinue
    $sweep = Get-CimInstance Win32_Process -Filter "Name='node.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -match 'autogold[_-]sweep' }
    if (-not $hleval -and -not $sweep) { break }
    Start-Sleep -Seconds 20
}
Start-Sleep -Seconds 15
Start-Process powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','D:\upload\continuous_training.ps1' -WindowStyle Hidden
