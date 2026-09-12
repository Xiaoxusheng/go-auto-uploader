package app

import "testing"

func TestIsRemoteNameConflict(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"文件名冲突，同一目录下已存在同名文件(00010327)", true},
		{"同名文件已存在", true},
		{"file name conflict", true},
		{"File Already Exists", true},
		{"", false},
		{"空间不足", false},
		{"unauthorized", false},
	}
	for _, c := range cases {
		if got := isRemoteNameConflict(c.msg); got != c.want {
			t.Fatalf("isRemoteNameConflict(%q)=%v want %v", c.msg, got, c.want)
		}
	}
}
