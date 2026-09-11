package notification

import (
	"strings"
	"testing"
)

// 占位符必须被识别为「未配置」，否则会拿默认模板去连 SMTP。
func TestEmailConfigReady(t *testing.T) {
	bad := EmailConfig{Host: "smtp.qq.com", Port: 587, From: "your_email@qq.com", AuthCode: "your_auth_code", To: "receive_email@qq.com"}
	if bad.Ready() {
		t.Fatal("占位符配置不应视为可用")
	}
	if (EmailConfig{}).Ready() {
		t.Fatal("空配置不应视为可用")
	}
	good := EmailConfig{Host: "smtp.qq.com", Port: 587, From: "a@qq.com", AuthCode: "abcd1234", To: "b@qq.com"}
	if !good.Ready() {
		t.Fatal("完整配置应可用")
	}
	noHost := good
	noHost.Host = ""
	if noHost.Ready() {
		t.Fatal("缺 SMTP 主机不应可用")
	}
}

func TestEncodeMIMEHeader(t *testing.T) {
	if got := EncodeMIMEHeader("Upload Report"); got != "Upload Report" {
		t.Fatalf("纯 ASCII 不应编码: %q", got)
	}
	got := EncodeMIMEHeader("📦 上传成功报告")
	if !strings.HasPrefix(got, "=?UTF-8?B?") || !strings.HasSuffix(got, "?=") {
		t.Fatalf("中文主题应做 RFC2047 编码: %q", got)
	}
}

func TestSplitRecipients(t *testing.T) {
	got := splitRecipients("a@x.com, b@y.com;c@z.com")
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	if len(splitRecipients("  ")) != 0 {
		t.Fatal("空白应收敛为空")
	}
}

func TestBuildMIMEMessage(t *testing.T) {
	m := string(buildMIMEMessage("a@qq.com", "b@qq.com", "📦 报告", "<b>hi</b>"))
	for _, want := range []string{
		"From: a@qq.com",
		"To: b@qq.com",
		"Subject: =?UTF-8?B?",
		"Date: ",
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"Content-Transfer-Encoding: base64",
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("邮件头缺少 %q:\n%s", want, m)
		}
	}
}

// 未配置时邮件通道应静默跳过，不报错也不阻塞其它通道。
func TestDynamicEmailSkipsWhenUnconfigured(t *testing.T) {
	d := &DynamicEmail{CfgFn: func() EmailConfig { return EmailConfig{} }}
	if err := d.Notify(nil, Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("未配置应静默跳过, got %v", err)
	}
	if err := (&DynamicEmail{}).Notify(nil, Message{}); err != nil {
		t.Fatalf("无 CfgFn 应静默跳过, got %v", err)
	}
}
