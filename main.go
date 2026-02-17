package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/* ================= 全局配置 ================= */

var (
	dirs             string
	server           string
	workers          int
	rateMB           int
	dayRateMB        int
	nightRateMB      int
	reportMinutes    int
	scanningInterval int

	username = "admin"
	password = "LilKmxNF"
	otpCode  = "123456"

	token   string
	tokenMu sync.Mutex

	httpCli = &http.Client{Timeout: 0}

	hashFile = "uploaded_hash.db"

	queueCount   int64
	activeWorker int64
)

const (
	safeBaseDir    = "/_safe_uploads"
	successLogFile = "upload_success.json"

	mailFrom     = "2673893724@qq.com"
	mailAuthCode = "koeekvlajhtsdije"
	mailTo       = "3096407768@qq.com"
)

/* ================= main ================= */

func main() {
	flag.StringVar(&dirs, "dirs", "", "扫描目录(逗号分隔)")
	flag.StringVar(&server, "server", "http://127.0.0.1:5244", "服务器")
	flag.IntVar(&workers, "workers", 3, "并发")
	flag.IntVar(&rateMB, "rate", 0, "手动限速 MB/s")
	flag.IntVar(&dayRateMB, "day-rate", 20, "白天限速 MB/s")
	flag.IntVar(&nightRateMB, "night-rate", 80, "夜晚限速 MB/s")
	flag.IntVar(&scanningInterval, "night-rate", 30, "默认30min扫描一次")
	flag.IntVar(&reportMinutes, "report-minutes", 360, "邮件统计分钟")
	flag.Parse()

	if dirs == "" {
		log.Fatal("必须指定 -dirs")
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	go queueStatusLoop()
	go reportLoop()

	for {
		if err := login(); err != nil {
			log.Println("[LOGIN][ERR]", err)
			time.Sleep(30 * time.Second)
			continue
		}
		runOnce()
		time.Sleep(time.Minute * time.Duration(scanningInterval))
	}
}

/* ================= 队列状态 ================= */

func queueStatusLoop() {
	start := time.Now()
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		log.Printf("[QUEUE][STATUS] 运行:%s 等待:%d 工作中:%d 并发:%d",
			time.Since(start).Truncate(time.Second),
			atomic.LoadInt64(&queueCount),
			atomic.LoadInt64(&activeWorker),
			workers,
		)
	}
}

/* ================= 扫描 & worker ================= */

func runOnce() {
	taskCh := make(chan string, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for path := range taskCh {
				atomic.AddInt64(&queueCount, -1)
				atomic.AddInt64(&activeWorker, 1)

				start := time.Now()
				handleFile(path)
				log.Printf("[UPLOAD][DONE][W%d] %s 耗时:%s",
					id, path, time.Since(start).Truncate(time.Millisecond))

				atomic.AddInt64(&activeWorker, -1)
			}
		}(i + 1)
	}
	for _, root := range strings.Split(dirs, ",") {
		root = strings.TrimSpace(root)
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			atomic.AddInt64(&queueCount, 1)
			taskCh <- path
			return nil
		})
	}

	close(taskCh)
	wg.Wait()
}

/* ================= 文件处理 ================= */

func handleFile(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	hash := fileHash(path)
	if hashExists(hash) {
		log.Println("[SKIP][HASH]", path)
		return
	}

	// ✅ 修复后的 root 识别
	root := detectRoot(path)
	if root == "" {
		log.Println("[SKIP][NO_ROOT_MATCH]", path)
		return
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		log.Println("[PATH][REL][ERR]", err, path)
		return
	}

	dir := filepath.Dir(rel)
	name := cleanFileName(filepath.Base(rel))

	remote := filepath.ToSlash(filepath.Join(safeBaseDir, dir, name))

	if upload(path, remote, info.Size()) {
		saveHash(hash)
		os.Remove(path)
		recordSuccess(remote, name, info.Size())
	}
}

/* ================= 上传 ================= */

func upload(local, remote string, size int64) bool {
	f, err := os.Open(local)
	if err != nil {
		return false
	}
	defer f.Close()

	pr := NewProgressReader(filepath.Base(remote), f, size, currentRate())

	req, _ := http.NewRequest("PUT", server+"/api/fs/put", pr)
	req.ContentLength = size
	req.Header.Set("File-Path", remote)
	req.Header.Set("Content-Type", "application/octet-stream")

	tokenMu.Lock()
	req.Header.Set("Authorization", token)
	tokenMu.Unlock()

	resp, err := httpCli.Do(req)
	if err != nil {
		log.Println("[HTTP][ERR]", err)
		return false
	}
	defer resp.Body.Close()

	var r struct{ Code int }
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Code == 200
}

/* ================= 进度 & 限速 ================= */

type ProgressReader struct {
	name  string
	r     io.Reader
	total int64
	read  int64
	rate  int64
	last  time.Time
	start time.Time
}

func NewProgressReader(name string, r io.Reader, total int64, rateMB int) *ProgressReader {
	var rate int64
	if rateMB > 0 {
		rate = int64(rateMB) * 1024 * 1024
	}
	return &ProgressReader{
		name:  name,
		r:     r,
		total: total,
		rate:  rate,
		start: time.Now(),
	}
}

func (p *ProgressReader) Read(b []byte) (int, error) {
	startRead := time.Now()
	n, err := p.r.Read(b)
	p.read += int64(n)

	if p.rate > 0 {
		expect := time.Duration(int64(time.Second) * int64(n) / p.rate)
		if d := time.Since(startRead); d < expect {
			time.Sleep(expect - d)
		}
	}

	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		log.Printf("[UPLOAD][PROGRESS] %s %.1f%% 用时:%s",
			p.name,
			float64(p.read)*100/float64(p.total),
			time.Since(p.start).Truncate(time.Second),
		)
	}
	return n, err
}

func currentRate() int {
	if rateMB > 0 {
		return rateMB
	}
	h := time.Now().Hour()
	if h >= 8 && h < 23 {
		return dayRateMB
	}
	return nightRateMB
}

/* ================= 文件名清洗 ================= */

func cleanFileName(name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)

	var b strings.Builder
	lastDash := false

	for _, r := range base {
		if isChinese(r) || isAlphaNum(r) {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}

	res := strings.Trim(b.String(), "-")
	if res == "" {
		res = "file"
	}
	return res + ext
}

func isChinese(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF
}

func isAlphaNum(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

/* ================= 成功记录 ================= */

type UploadRecord struct {
	Time     time.Time
	Streamer string
	Name     string
	Remote   string
	Size     int64
}

func recordSuccess(remote, name string, size int64) {
	var list []UploadRecord
	data, _ := os.ReadFile(successLogFile)
	json.Unmarshal(data, &list)

	list = append(list, UploadRecord{
		Time:     time.Now(),
		Streamer: detectStreamer(remote),
		Name:     name,
		Remote:   remote,
		Size:     size,
	})

	b, _ := json.MarshalIndent(list, "", "  ")
	os.WriteFile(successLogFile, b, 0644)
}

/* ================= 邮件系统 ================= */

func reportLoop() {
	ticker := time.NewTicker(time.Duration(reportMinutes) * time.Minute)
	for range ticker.C {
		sendReport()
	}
}

func sendReport() {
	data, _ := os.ReadFile(successLogFile)
	var list []UploadRecord
	if json.Unmarshal(data, &list) != nil || len(list) == 0 {
		return
	}

	/* ===== 分组 + 流量统计 ===== */

	group := map[string][]UploadRecord{}
	var totalBytes int64

	for _, r := range list {
		group[r.Streamer] = append(group[r.Streamer], r)
		totalBytes += r.Size
	}

	totalMB := float64(totalBytes) / 1024 / 1024
	now := time.Now().Format("2006-01-02 15:04")

	/* ===== 邮件 HTML（table 版，邮件客户端安全） ===== */

	var html strings.Builder

	html.WriteString(`
<table width="100%" cellpadding="0" cellspacing="0" style="background:#f4f6f8;padding:24px;">
<tr>
<td align="center">

<table width="760" cellpadding="0" cellspacing="0"
       style="background:#ffffff;border-radius:12px;
              font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial;">
`)

	/* ===== 标题 ===== */

	html.WriteString(fmt.Sprintf(`
<tr>
<td style="padding:24px;border-bottom:1px solid #e5e7eb;">
<h2 style="margin:0;font-size:20px;color:#111827;">
📦 上传成功报告
</h2>
<p style="margin:6px 0 0;font-size:13px;color:#6b7280;">
最近 %d 分钟 ｜ 生成时间 %s
</p>
</td>
</tr>
`, reportMinutes, now))

	/* ===== 统计卡片 ===== */

	html.WriteString(fmt.Sprintf(`
<tr>
<td style="padding:20px;">
<table width="100%%" cellpadding="12" cellspacing="0"
       style="background:#f8fafc;border-radius:10px;">
<tr>
<td>
<div style="font-size:12px;color:#6b7280;">文件数量</div>
<div style="font-size:22px;color:#111827;"><b>%d</b></div>
</td>
<td>
<div style="font-size:12px;color:#6b7280;">消耗流量</div>
<div style="font-size:22px;color:#111827;"><b>%.2f MB</b></div>
</td>
<td>
<div style="font-size:12px;color:#6b7280;">主播数量</div>
<div style="font-size:22px;color:#111827;"><b>%d</b></div>
</td>
</tr>
</table>
</td>
</tr>
`, len(list), totalMB, len(group)))

	/* ===== 明细 ===== */

	for streamer, files := range group {
		html.WriteString(fmt.Sprintf(`
<tr>
<td style="padding:20px 20px 8px 20px;">
<h3 style="margin:0;font-size:15px;color:#2563eb;">
🎬 %s
</h3>
</td>
</tr>

<tr>
<td style="padding:0 20px 20px 20px;">
<table width="100%%" cellpadding="8" cellspacing="0"
       style="border-collapse:collapse;font-size:13px;">
<tr style="background:#f1f5f9;color:#374151;">
<th align="left">时间</th>
<th align="left">文件名</th>
<th align="right">大小</th>
<th align="left">存储路径</th>
</tr>
`, streamer))

		for _, f := range files {
			html.WriteString(fmt.Sprintf(`
<tr style="border-bottom:1px solid #e5e7eb;">
<td style="color:#6b7280;">%s</td>
<td style="color:#111827;font-weight:500;">%s</td>
<td align="right">%.2f MB</td>
<td style="font-family:ui-monospace,Menlo,monospace;
           word-break:break-all;color:#374151;">
%s
</td>
</tr>
`,
				f.Time.Format("01-02 15:04"),
				f.Name,
				float64(f.Size)/1024/1024,
				f.Remote,
			))
		}

		html.WriteString(`
</table>
</td>
</tr>
`)
	}

	/* ===== 页脚 ===== */

	html.WriteString(`
<tr>
<td style="padding:16px 24px;border-top:1px dashed #e5e7eb;
           font-size:12px;color:#9ca3af;">
本邮件由自动上传系统生成，请勿回复
</td>
</tr>

</table>
</td>
</tr>
</table>
`)

	sendQQMail("📦 上传成功报告", html.String())

	// 清空记录，防止重复统计
	_ = os.WriteFile(successLogFile, []byte("[]"), 0644)
}

/* ================= 工具 ================= */

func login() error {
	body := fmt.Sprintf(`{"username":"%s","password":"%s","otp_code":"%s"}`, username, password, otpCode)
	req, _ := http.NewRequest("POST", server+"/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r struct {
		Code int
		Data struct{ Token string }
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Code != 200 {
		return fmt.Errorf("login failed")
	}

	tokenMu.Lock()
	token = r.Data.Token
	tokenMu.Unlock()
	return nil
}

func fileHash(p string) string {
	f, _ := os.Open(p)
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func hashExists(h string) bool {
	data, _ := os.ReadFile(hashFile)
	return strings.Contains(string(data), h)
}

func saveHash(h string) {
	f, _ := os.OpenFile(hashFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	f.WriteString(h + "\n")
}

/* ================= 核心修复函数 ================= */

func detectRoot(path string) string {
	path = filepath.Clean(path)

	for _, d := range strings.Split(dirs, ",") {
		root := filepath.Clean(strings.TrimSpace(d))

		rel, err := filepath.Rel(root, path)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return root
		}
	}
	return ""
}

func detectStreamer(remote string) string {
	parts := strings.Split(remote, "/")
	if len(parts) > 2 {
		return parts[2]
	}
	return "未知"
}

func sendQQMail(subject, body string) {
	msg := []byte(
		"To: " + mailTo + "\r\n" +
			"From: " + mailFrom + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
			body,
	)
	auth := smtp.PlainAuth("", mailFrom, mailAuthCode, "smtp.qq.com")
	smtp.SendMail("smtp.qq.com:587", auth, mailFrom, []string{mailTo}, msg)
}
