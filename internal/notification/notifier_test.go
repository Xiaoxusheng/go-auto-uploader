package notification

import (
	"context"
	"strings"
	"testing"
)

func TestHubFanOut(t *testing.T) {
	h := New()
	var got []string
	h.Register(FuncNotifier{Label: "a", Fn: func(_ context.Context, m Message) error {
		got = append(got, "a:"+m.Title)
		return nil
	}})
	h.Register(FuncNotifier{Label: "b", Fn: func(_ context.Context, m Message) error {
		got = append(got, "b:"+m.Title)
		return nil
	}})
	h.NotifyAll(context.Background(), Message{Title: "hi", Body: "x"})
	if h.Len() != 2 || len(got) != 2 {
		t.Fatalf("fanout failed len=%d got=%v", h.Len(), got)
	}
}

func TestCleanWeChatTitle(t *testing.T) {
	if CleanWeChatTitle("▶️ 开播通知") != "开播通知" {
		t.Fatal("emoji strip")
	}
}

func TestBuildWeChatCardHTML(t *testing.T) {
	html := BuildWeChatCardHTML("开播通知", "line1\nline2")
	if !strings.Contains(html, "line1<br>line2") {
		t.Fatal("newline to br")
	}
	if !strings.Contains(html, "svg") {
		t.Fatal("missing icon")
	}
}

func TestDynamicPushPlusEmptyToken(t *testing.T) {
	n := &DynamicPushPlus{TokenFn: func() string { return "" }}
	if err := n.Notify(context.Background(), Message{Title: "t"}); err != nil {
		t.Fatal(err)
	}
}
