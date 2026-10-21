#!/bin/bash
# ============================================================
# 重启 uploader 服务（systemd 管理版，2026-10-09 起）
#
# 用法（本地）：
#   tr -d '\r' < deploy_server/restart_cwd.sh | ssh xyx.homes "bash -s"
#
# 说明：本脚本取代旧的「kill + nohup 按 .restart_args.txt 重启」做法。
#       服务由 systemd 唯一管理（Restart=always + 开机自启），
#       WorkingDirectory=/home/upload 由 unit 保证，不再需要手工 cd。
#       （旧版必须 cd /home/upload，否则相对路径全偏 =「主播数据没来」）
# ============================================================
set -e

echo "=== 重启 uploader.service ==="
systemctl restart uploader.service
sleep 12

echo "is-active: $(systemctl is-active uploader.service)"
PID=$(systemctl show -p MainPID --value uploader.service)
echo "MainPID: $PID"
[ -n "$PID" ] && echo "CWD: $(readlink /proc/$PID/cwd 2>/dev/null)"
echo "--- 端口 ---"
ss -tlnp 2>/dev/null | grep 8888 | head -1
echo "--- 最近日志 ---"
journalctl -u uploader.service --no-pager -n 8
