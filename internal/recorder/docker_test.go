package recorder

import "testing"

func TestStatusEmpty(t *testing.T) {
	d := &DockerController{ContainerNameFn: func() string { return "" }}
	if d.Status() != "未配置" {
		t.Fatal(d.Status())
	}
}

func TestControlInvalid(t *testing.T) {
	d := &DockerController{
		ContainerNameFn: func() string { return "x" },
		Exec: func(name string, args ...string) ([]byte, error) {
			return nil, nil
		},
	}
	if err := d.Control("kill"); err == nil {
		t.Fatal("invalid action should fail")
	}
}

func TestControlAndLogs(t *testing.T) {
	var last []string
	d := &DockerController{
		ContainerNameFn: func() string { return "app" },
		Exec: func(name string, args ...string) ([]byte, error) {
			last = append([]string{name}, args...)
			return []byte("running\n"), nil
		},
	}
	if d.Status() != "running" {
		t.Fatal(d.Status())
	}
	if err := d.Control("restart"); err != nil {
		t.Fatal(err)
	}
	if last[1] != "restart" || last[2] != "app" {
		t.Fatalf("args %v", last)
	}
	if _, err := d.Logs(50); err != nil {
		t.Fatal(err)
	}
	if last[1] != "logs" {
		t.Fatalf("logs args %v", last)
	}
}
