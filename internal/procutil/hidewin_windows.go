//go:build windows

package procutil

import (
	"os/exec"
	"syscall"
)

// HideWindow 让子进程（ffmpeg 等控制台程序）不弹任何窗口。
//
// 仅 CREATE_NO_WINDOW 在 Win11「默认终端 = Windows Terminal」时仍可能弹黑窗，
// 必须叠加 STARTF_USESHOWWINDOW + SW_HIDE（Go 的 HideWindow）。
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
