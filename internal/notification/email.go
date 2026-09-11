package notification

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// EmailConfig SMTP 邮件通道配置（由调用方从统一配置注入，支持热更新）。
type EmailConfig struct {
	Host     string
	Port     int
	From     string
	AuthCode string
	To       string
}

// placeholderValues 默认模板里的占位值，命中即视为「未配置」。
var placeholderValues = map[string]bool{
	"your_email@qq.com":    true,
	"your_auth_code":       true,
	"receive_email@qq.com": true,
	"":                     true,
}

// isPlaceholder 判定是否为未填写的占位值。
func isPlaceholder(s string) bool {
	v := strings.ToLower(strings.TrimSpace(s))
	if placeholderValues[v] {
		return true
	}
	return strings.Contains(v, "your_") || strings.Contains(v, "your-") ||
		strings.Contains(v, "example.com") || strings.Contains(v, "receive_email")
}

// Ready 判断邮件配置是否可用（字段齐全且不是占位符）。
func (c EmailConfig) Ready() bool {
	if isPlaceholder(c.From) || isPlaceholder(c.AuthCode) || isPlaceholder(c.To) {
		return false
	}
	if strings.TrimSpace(c.Host) == "" {
		return false
	}
	return true
}

// normalize 补默认端口。
func (c EmailConfig) normalize() EmailConfig {
	if c.Port <= 0 {
		c.Port = 587
	}
	return c
}

// splitRecipients 支持逗号/分号分隔的多个收件人。
func splitRecipients(to string) []string {
	fields := strings.FieldsFunc(to, func(r rune) bool { return r == ',' || r == ';' })
	var out []string
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// SendEmail 通过 SMTP 发送 HTML 邮件。
// 465 走隐式 TLS；其它端口在服务端宣告 STARTTLS 时自动升级（QQ 邮箱 587 即为此模式）。
func SendEmail(ctx context.Context, cfg EmailConfig, subject, htmlBody string) error {
	if !cfg.Ready() {
		return fmt.Errorf("邮件配置不完整或仍为占位符")
	}
	cfg = cfg.normalize()
	host := cfg.Host
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", cfg.Port))
	tlsCfg := &tls.Config{ServerName: host}

	d := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if cfg.Port == 465 {
		conn, err = tls.DialWithDialer(d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()

	if cfg.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("STARTTLS 失败: %w", err)
			}
		}
	}

	if err := client.Auth(smtp.PlainAuth("", cfg.From, cfg.AuthCode, host)); err != nil {
		return fmt.Errorf("SMTP 鉴权失败（QQ 邮箱需使用授权码而非登录密码）: %w", err)
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	for _, rcpt := range splitRecipients(cfg.To) {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}

	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMIMEMessage(cfg.From, cfg.To, subject, htmlBody)); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// buildMIMEMessage 组装 MIME 邮件；主题做 RFC 2047 编码，正文 base64 传输。
func buildMIMEMessage(from, to, subject, htmlBody string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + EncodeMIMEHeader(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	b.WriteString(wrapBase64(base64.StdEncoding.EncodeToString([]byte(htmlBody))))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// EncodeMIMEHeader 含非 ASCII 的头字段编码为 RFC 2047（否则中文主题会乱码/被拒）。
func EncodeMIMEHeader(s string) string {
	ascii := true
	for _, r := range s {
		if r > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// wrapBase64 每 76 字符换行，符合 RFC 2045。
func wrapBase64(s string) string {
	const width = 76
	var b strings.Builder
	for len(s) > width {
		b.WriteString(s[:width])
		b.WriteString("\r\n")
		s = s[width:]
	}
	b.WriteString(s)
	return b.String()
}

// DynamicEmail 配置可热更新的邮件通道。
type DynamicEmail struct {
	CfgFn func() EmailConfig
}

// Name 通道名。
func (d *DynamicEmail) Name() string { return "email" }

// Notify 发送通知邮件；未配置时静默跳过（不阻塞其它通道）。
func (d *DynamicEmail) Notify(ctx context.Context, msg Message) error {
	if d.CfgFn == nil {
		return nil
	}
	cfg := d.CfgFn()
	if !cfg.Ready() {
		return nil
	}
	return SendEmail(ctx, cfg, CleanWeChatTitle(msg.Title), BuildWeChatCardHTML(msg.Title, msg.Body))
}
