//go:build !windows

package procutil

import "os/exec"

// HideWindow 在非 Windows 上为 no-op。
func HideWindow(cmd *exec.Cmd) {}
