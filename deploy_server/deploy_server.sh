#!/bin/bash
# ============================================================
# douyinliverecorder 新版（A档高光 + minac1 0.15）服务器部署脚本
# 策略：只替换二进制，**不碰服务器上已有的 config/数据/下载目录**。
# 姿态门在纯 Go 构建中编译剔除（自动关闭）——服务器按用户要求保持纯 Go。
#
# 用法（在服务器上，与 uploader_linux 同目录执行）：
#   bash deploy_server.sh
# ============================================================
set -e
echo "=== 1. 探测现有部署 ==="
OLD=$(pgrep -f 'uploader' | head -5)
OLD_BIN=$(command -v uploader 2>/dev/null || true)
for d in /root/douyinliverecorder /opt/douyinliverecorder /root/douyin; do
  [ -d "$d" ] && echo "发现部署目录: $d" && ls -la "$d" | head -8
done
find / -maxdepth 4 -name 'uploader*' -type f 2>/dev/null | grep -v proc | head -5
echo "运行中的 uploader PID: ${OLD:-无}"

echo "=== 2. 停旧进程 ==="
[ -n "$OLD" ] && kill $OLD 2>/dev/null && sleep 3
pgrep -f uploader >/dev/null && { pkill -9 -f uploader; sleep 2; }
echo "旧进程已停"

echo "=== 3. 备份并替换二进制 ==="
if [ -n "$OLD_BIN" ]; then
  cp "$OLD_BIN" "${OLD_BIN}.bak.$(date +%s)"
  install -m 755 uploader_linux "$OLD_BIN"
  BIN="$OLD_BIN"
else
  install -m 755 uploader_linux /usr/local/bin/uploader
  BIN="/usr/local/bin/uploader"
fi
echo "新二进制: $BIN"

echo "=== 4. 启动 ==="
echo "⚠️ 未自动启动：请确认服务器上原启动方式（systemd/nohup/docker）后手动拉起，"
echo "   例如: nohup $BIN -web-port 8080 > uploader.log 2>&1 &"
echo "完成。"
