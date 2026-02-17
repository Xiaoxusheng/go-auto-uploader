#!/bin/bash
set -e

SERVICE="uploader"

echo "▶ 停止服务..."
systemctl stop $SERVICE

echo "⏳ 等待进程完全退出..."
sleep 2

echo "▶ 启动服务..."
systemctl start $SERVICE

echo "✅ 服务已重启"
systemctl status $SERVICE --no-pager
