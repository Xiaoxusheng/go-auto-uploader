package app

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"time"

	"upload/internal/storage"
)

// reportLoop 邮件报告定时循环。
func reportLoop() {
	intervalMinutes := AppCfg().EmailInterval
	nextReportTime := time.Now().Add(time.Duration(intervalMinutes) * time.Minute)

	for {
		sleepDuration := time.Until(nextReportTime)
		if sleepDuration <= 0 {
			sendReport()
			intervalMinutes = AppCfg().EmailInterval
			nextReportTime = time.Now().Add(time.Duration(intervalMinutes) * time.Minute)
			continue
		}

		select {
		case <-time.After(sleepDuration):
		case <-TriggerReportCh:
			log.Printf("[SYSTEM] 📧 配置发生变更，但这不会打断原有的邮件倒计时，邮件仍将在 %v 后发送", time.Until(nextReportTime).Truncate(time.Second))
		}
	}
}

func sendReport() {
	list := SuccessStore.Snapshot()
	if len(list) == 0 {
		return
	}

	repMinutes := AppCfg().EmailInterval
	cutoffTime := time.Now().Add(-time.Duration(repMinutes) * time.Minute)
	var recentList []storage.UploadRecord
	for _, r := range list {
		if r.Time.After(cutoffTime) {
			recentList = append(recentList, r)
		}
	}
	if len(recentList) == 0 {
		log.Printf("[REPORT] 📦 过去 %d 分钟内无新上传成功记录，跳过本次邮件推送", repMinutes)
		return
	}

	group := map[string][]storage.UploadRecord{}
	var totalBytes int64
	for _, r := range recentList {
		group[r.Streamer] = append(group[r.Streamer], r)
		totalBytes += r.Size
	}

	totalMB := float64(totalBytes) / 1024 / 1024
	now := time.Now().Format("2006-01-02 15:04")

	var html strings.Builder
	html.WriteString(`
<table width="100%" cellpadding="0" cellspacing="0" style="background:#f4f6f8;padding:24px;">
<tr><td align="center"><table width="760" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:12px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial;">
`)
	html.WriteString(fmt.Sprintf(`
<tr><td style="padding:24px;border-bottom:1px solid #e5e7eb;">
<h2 style="margin:0;font-size:20px;color:#111827;">📦 上传成功报告</h2>
<p style="margin:6px 0 0;font-size:13px;color:#6b7280;">统计周期 %d 分钟 ｜ 生成时间 %s</p>
</td></tr>
`, repMinutes, now))

	html.WriteString(fmt.Sprintf(`
<tr><td style="padding:20px;">
<table width="100%%" cellpadding="12" cellspacing="0" style="background:#f8fafc;border-radius:10px;">
<tr>
<td><div style="font-size:12px;color:#6b7280;">新增文件数</div><div style="font-size:22px;color:#111827;"><b>%d</b></div></td>
<td><div style="font-size:12px;color:#6b7280;">消耗流量</div><div style="font-size:22px;color:#111827;"><b>%.2f MB</b></div></td>
<td><div style="font-size:12px;color:#6b7280;">涉及主播数</div><div style="font-size:22px;color:#111827;"><b>%d</b></div></td>
</tr>
</table></td></tr>
`, len(recentList), totalMB, len(group)))

	for streamer, files := range group {
		html.WriteString(fmt.Sprintf(`<tr><td style="padding:20px 20px 8px 20px;"><h3 style="margin:0;font-size:15px;color:#2563eb;">🎬 %s</h3></td></tr>
<tr><td style="padding:0 20px 20px 20px;"><table width="100%%" cellpadding="8" cellspacing="0" style="border-collapse:collapse;font-size:13px;">
<tr style="background:#f1f5f9;color:#374151;"><th align="left">时间</th><th align="left">文件名</th><th align="right">大小</th><th align="left">存储路径</th></tr>
`, streamer))
		for _, f := range files {
			html.WriteString(fmt.Sprintf(`<tr style="border-bottom:1px solid #e5e7eb;"><td style="color:#6b7280;">%s</td><td style="color:#111827;font-weight:500;">%s</td><td align="right">%.2f MB</td><td style="font-family:ui-monospace,Menlo,monospace;word-break:break-all;color:#374151;">%s</td></tr>`,
				f.Time.Format("01-02 15:04"), f.Name, float64(f.Size)/1024/1024, f.Remote,
			))
		}
		html.WriteString(`</table></td></tr>`)
	}

	html.WriteString(`<tr><td style="padding:16px 24px;border-top:1px dashed #e5e7eb;font-size:12px;color:#9ca3af;">本邮件由自动上传系统生成，请勿回复</td></tr></table></td></tr></table>`)

	log.Printf("[REPORT] 📤 正在发送本周期统计邮件，包含 %d 个文件记录", len(recentList))
	sendQQMail("📦 上传成功报告", html.String())
}

func sendQQMail(subject, body string) {
	cfg := AppCfg()
	mailFrom, mailAuthCode, mailTo := cfg.MailFrom, cfg.MailAuthCode, cfg.MailTo
	if mailFrom == "" || mailAuthCode == "" || mailTo == "" {
		log.Printf("[REPORT][MAIL] ⚠️ 邮件参数未配置或不完整，自动跳过邮件发送")
		return
	}

	msg := []byte(
		"To: " + mailTo + "\r\n" +
			"From: " + mailFrom + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
			body,
	)
	auth := smtp.PlainAuth("", mailFrom, mailAuthCode, "smtp.qq.com")
	if err := smtp.SendMail("smtp.qq.com:587", auth, mailFrom, []string{mailTo}, msg); err != nil {
		log.Printf("[REPORT][MAIL][ERR] 邮件发送失败: %v", err)
	}
}
