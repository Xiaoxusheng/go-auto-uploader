// Package recorder 管理外部 Docker 录制引擎的探测与容器控制。
// 内置录制仍留在 builtin_recorder.go（体量大，后续再拆）。
package recorder

import (
	"fmt"
	"os/exec"
	"strings"
)

// DockerController 通过宿主机 docker CLI 控制容器。
type DockerController struct {
	// ContainerNameFn 返回目标容器名（可热更新配置）。
	ContainerNameFn func() string
	// Exec 可注入测试用命令执行器；nil 时用 os/exec。
	Exec func(name string, args ...string) ([]byte, error)
}

func (d *DockerController) run(args ...string) ([]byte, error) {
	if d.Exec != nil {
		return d.Exec("docker", args...)
	}
	return exec.Command("docker", args...).CombinedOutput()
}

func (d *DockerController) container() string {
	if d.ContainerNameFn == nil {
		return ""
	}
	return strings.TrimSpace(d.ContainerNameFn())
}

// Status 返回容器状态字符串；未配置或失败时返回友好文案。
func (d *DockerController) Status() string {
	c := d.container()
	if c == "" {
		return "未配置"
	}
	out, err := d.run("inspect", "-f", "{{.State.Status}}", c)
	if err != nil {
		return "离线/异常"
	}
	return strings.TrimSpace(string(out))
}

// Control 执行 start/stop/restart。
func (d *DockerController) Control(action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("非法的控制指令: %s", action)
	}
	c := d.container()
	if c == "" {
		return fmt.Errorf("未配置容器名")
	}
	out, err := d.run(action, c)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w / %s", action, c, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Logs 拉取容器尾部日志。
func (d *DockerController) Logs(tail int) (string, error) {
	c := d.container()
	if c == "" {
		return "", fmt.Errorf("未配置容器名")
	}
	if tail <= 0 {
		tail = 100
	}
	out, err := d.run("logs", "--tail", fmt.Sprintf("%d", tail), c)
	if err != nil {
		return "", fmt.Errorf("docker logs: %w / %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
