package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// PushPlusNotifier 调用 PushPlus 微信推送。
type PushPlusNotifier struct {
	Token  string
	Client *http.Client
}

// Name 通道名。
func (p *PushPlusNotifier) Name() string { return "wechat-pushplus" }

// TokenFn 动态取 Token（配置热更新）。
type TokenFn func() string

// DynamicPushPlus Token 可热更新的微信通道。
type DynamicPushPlus struct {
	TokenFn TokenFn
	Client  *http.Client
}

func (d *DynamicPushPlus) Name() string { return "wechat-pushplus" }

func (d *DynamicPushPlus) Notify(ctx context.Context, msg Message) error {
	if d.TokenFn == nil {
		return nil
	}
	token := d.TokenFn()
	if token == "" {
		return nil
	}
	cli := d.Client
	if cli == nil {
		cli = &http.Client{Timeout: 15 * time.Second}
	}
	html := BuildWeChatCardHTML(msg.Title, msg.Body)
	body, _ := json.Marshal(map[string]string{
		"token":    token,
		"title":    CleanWeChatTitle(msg.Title),
		"content":  html,
		"template": "html",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://www.pushplus.plus/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// CleanWeChatTitle 去掉调用端 emoji 前缀。
func CleanWeChatTitle(title string) string {
	title = strings.ReplaceAll(title, "▶️ ", "")
	title = strings.ReplaceAll(title, "⏹️ ", "")
	title = strings.ReplaceAll(title, "🛑 ", "")
	title = strings.ReplaceAll(title, "⏸️ ", "")
	return title
}

// BuildWeChatCardHTML 按标题语义选择 SVG 图标并拼卡片。
func BuildWeChatCardHTML(title, body string) string {
	iconColor := "#3B82F6"
	iconBg := "#EFF6FF"
	var svgIcon string

	switch {
	case strings.Contains(title, "开播"):
		iconColor, iconBg = "#10B981", "#ECFDF5"
		svgIcon = `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 256 256"><path fill="currentColor" d="M168,128a40,40,0,1,1-40-40A40,40,0,0,1,168,128Zm40-8a8,8,0,0,0-8,8,72,72,0,0,1-72,72,8,8,0,0,0,0,16,88.1,88.1,0,0,0,88-88A8,8,0,0,0,208,120Zm48,8a136.15,136.15,0,0,1-136,136,8,8,0,0,1,0-16,120.14,120.14,0,0,0,120-120,8,8,0,0,1,16,0ZM72,128a72,72,0,0,1,72-72,8,8,0,0,0,0-16,88.1,88.1,0,0,0-88,88,8,8,0,0,0,16,0ZM24,128A136.15,136.15,0,0,1,160,8a8,8,0,0,1,0,16A120.14,120.14,0,0,0,40,128a8,8,0,0,1-16,0Z"></path></svg>`
	case strings.Contains(title, "下播") || strings.Contains(title, "暂停"):
		iconColor, iconBg = "#F59E0B", "#FFFBEB"
		svgIcon = `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 256 256"><path fill="currentColor" d="M128,24A104,104,0,1,0,232,128,104.11,104.11,0,0,0,128,24Zm0,192a88,88,0,1,1,88-88A88.1,88.1,0,0,1,128,216Zm32-88a8,8,0,0,1-8,8H104a8,8,0,0,1,0-16h48A8,8,0,0,1,160,128Z"></path></svg>`
	case strings.Contains(title, "停止") || strings.Contains(title, "异常") || strings.Contains(title, "失败"):
		iconColor, iconBg = "#EF4444", "#FEF2F2"
		svgIcon = `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 256 256"><path fill="currentColor" d="M236.8,188.09,149.35,36.22h0a24.76,24.76,0,0,0-42.7,0L19.2,188.09a23.51,23.51,0,0,0,0,23.72A24.35,24.35,0,0,0,40.55,224h174.9a24.35,24.35,0,0,0,21.33-12.19A23.51,23.51,0,0,0,236.8,188.09ZM222.93,203.8a8.5,8.5,0,0,1-7.48,4.2H40.55a8.5,8.5,0,0,1-7.48-4.2,7.59,7.59,0,0,1,0-7.72L120.52,44.21a8.75,8.75,0,0,1,15,0l87.45,151.87A7.59,7.59,0,0,1,222.93,203.8ZM120,104v40a8,8,0,0,0,16,0V104a8,8,0,0,0-16,0Zm20,68a12,12,0,1,1-12-12A12,12,0,0,1,140,172Z"></path></svg>`
	default:
		svgIcon = `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 256 256"><path fill="currentColor" d="M128,24A104,104,0,1,0,232,128,104.11,104.11,0,0,0,128,24Zm0,192a88,88,0,1,1,88-88A88.1,88.1,0,0,1,128,216Zm16-40a8,8,0,0,1-8,8,16,16,0,0,1-16-16V128a8,8,0,0,1,0-16,16,16,0,0,1,16,16v40A8,8,0,0,1,144,176ZM112,84a12,12,0,1,1,12,12A12,12,0,0,1,112,84Z"></path></svg>`
	}

	formattedBody := strings.ReplaceAll(body, "\n", "<br>")
	currentTime := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf(`
	<div style="background: #ffffff; padding: 24px; border-radius: 16px; border: 1px solid #f3f4f6; box-shadow: 0 4px 20px rgba(0,0,0,0.03); font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif;">
		<div style="display: flex; align-items: center; margin-bottom: 20px;">
			<div style="display: flex; align-items: center; justify-content: center; width: 36px; height: 36px; border-radius: 10px; background-color: %s; color: %s; margin-right: 14px;">
				%s
			</div>
			<div style="font-size: 18px; font-weight: 600; color: #111827; letter-spacing: 0.3px;">%s</div>
		</div>
		<div style="font-size: 15px; color: #4b5563; line-height: 1.6; margin-bottom: 24px; letter-spacing: 0.2px;">
			%s
		</div>
		<div style="border-top: 1px solid #f3f4f6; padding-top: 16px; font-size: 12px; color: #9ca3af; display: flex; justify-content: space-between; align-items: center;">
			<span style="font-family: monospace;">%s</span>
			<span style="color: #d1d5db; font-weight: 500;">go-auto-uploader</span>
		</div>
	</div>
	`, iconBg, iconColor, svgIcon, CleanWeChatTitle(title), formattedBody, currentTime)
}
