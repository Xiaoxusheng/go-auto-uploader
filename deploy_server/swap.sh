#!/bin/bash
# ============================================================
# 服务器换装脚本（systemd 管理版，2026-10-09 起）
#
# 部署链路：本地 tar 打包 → ssh 传输 → 服务器 CGO 构建出 uploader.new
#           → 本脚本：stop 服务 → 备份并换装 → systemctl start → 健康检查
#
# 前置：/home/upload/uploader.new 已由上一步构建产出
# 用法（本地）：
#   tr -d '\r' < deploy_server/swap.sh | ssh xyx.homes "bash -s"
#
# ⚠️ 服务由 systemd（uploader.service）唯一管理，禁止再用 nohup 直拉：
#    systemd 有 Restart=always，手动另起进程会与它抢 8888 端口，
#    造成「每 10 秒重拉一个、每次都失败」的崩溃循环（2026-10-09 实际踩过）。
# ⚠️ 启动参数在 /etc/systemd/system/uploader.service（不再走 .restart_args.txt），
#    改参数要改 unit 文件后 systemctl daemon-reload。
# ============================================================
set -e

echo "=== 1. 停服务（systemd 唯一管理，避免双实例）==="
systemctl stop uploader.service || true
for i in $(seq 1 25); do
  systemctl is-active --quiet uploader.service || break
  sleep 1
done
echo "服务已停"

echo "=== 2. 备份并换装二进制 ==="
[ -f /home/upload/uploader.new ] || { echo "❌ 缺少 /home/upload/uploader.new，先跑构建步骤"; exit 1; }
cp -f /home/upload/uploader /home/upload/uploader.bak.$(date +%s)
cd /home/upload
mv -f /home/upload/uploader.new /home/upload/uploader
chmod 755 /home/upload/uploader
ls -la /home/upload/uploader

echo "=== 3. 启动 ==="
systemctl start uploader.service
sleep 12
echo "is-active: $(systemctl is-active uploader.service)"

echo "=== 4. 健康检查 ==="
ss -tlnp 2>/dev/null | grep 8888 | head -1
curl -s -o /dev/null -w "HTTP=%{http_code}\n" http://127.0.0.1:8888/
journalctl -u uploader.service --no-pager -n 5
