#!/bin/bash
set -e

APP_NAME="uploader"

echo "Building binary..."

CGO_ENABLED=0 go build -o "$APP_NAME" -ldflags "-w -s" ./cmd/uploader

echo "Build success!"
echo "Binary: ./$APP_NAME"
ls -lh "./$APP_NAME"
