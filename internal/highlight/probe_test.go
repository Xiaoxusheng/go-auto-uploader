package highlight

import (
	"strings"
	"testing"
)

// tailBuffer 只保留最后 N 行 —— ffmpeg 的报错压在末尾，
// 而每秒好几行的采样点输出会把它彻底淹没，不透传就只剩一个 exit status。
func TestTailBufferKeepsNewestLines(t *testing.T) {
	b := newTailBuffer(3)
	for _, s := range []string{"l1", "l2", "l3", "l4", "l5"} {
		b.add(s)
	}
	got := b.String()
	if got != "l3 / l4 / l5" {
		t.Fatalf("应保留最后 3 行且保持旧→新顺序，实际: %q", got)
	}
}

func TestTailBufferEdgeCases(t *testing.T) {
	if s := newTailBuffer(4).String(); s != "" {
		t.Fatalf("无内容时应返回空串，实际 %q", s)
	}

	// 容量 0 时不应留下任何内容
	z := newTailBuffer(0)
	z.add("x")
	if s := z.String(); s != "" {
		t.Fatalf("容量 0 时应返回空串，实际 %q", s)
	}

	// nil 接收者不能 panic（add/String 都可能被直接调用）
	var nilBuf *tailBuffer
	nilBuf.add("x")
	if s := nilBuf.String(); s != "" {
		t.Fatalf("nil 接收者应返回空串，实际 %q", s)
	}

	// 容量小于行数时，String 的分隔符要稳定，便于日志解析
	b := newTailBuffer(2)
	b.add("only-one")
	if s := b.String(); s != "only-one" {
		t.Fatalf("单行不应带分隔符，实际 %q", s)
	}
	if strings.Count(b.String(), " / ") != 0 {
		t.Fatalf("单行不应含分隔符，实际 %q", b.String())
	}
}
