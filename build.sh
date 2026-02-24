#!/bin/bash
set -e

APP_NAME="uploader"

echo "🚀 Building binary..."

# 当前系统编译
CGO_ENABLED=0 go build -o "$APP_NAME"  -ldflags "-w -s"

echo "✅ Build success!"
echo "📦 Binary: ./$APP_NAME"
echo "📏 Size:"
ls -lh "./$APP_NAME"
du -h "./$APP_NAME"

