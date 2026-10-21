//go:build !cgo

// videomae-probe 的纯 Go 构建占位：ONNX 推理依赖 cgo（onnxruntime 动态库），
// CGO_ENABLED=0 时保留子命令入口但直接报错退出——与 internal/pose 的
// gate_nocgo.go 同一策略，保证 `go build ./...` / `go test ./...` 在默认
// 纯 Go 构建下可用。
package main

import (
	"fmt"
	"os"
)

func cmdVideomaeProbe(args []string) {
	fmt.Fprintln(os.Stderr, "videomae-probe 需要 cgo 构建（CGO_ENABLED=1 + onnxruntime 动态库），当前二进制为纯 Go 构建")
	os.Exit(1)
}
