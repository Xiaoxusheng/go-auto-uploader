// Package config 收敛应用运行时配置：类型、加载、校验、原子保存与并发访问。
// JSON 字段名与历史 config.json 完全兼容，禁止为“分层好看”改字段名。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"upload/internal/fsutil"
)

// Config 是扁平的应用配置（与线上 config.json 一一对应）。
type Config struct {
	ScanInterval       int      `json:"scanInterval"`
	Workers            int      `json:"workers"`
	Rate               int      `json:"rate"`
	DayRate            int      `json:"dayRate"`
	NightRate          int      `json:"nightRate"`
	EmailInterval      int      `json:"emailInterval"`
	Running            bool     `json:"running"`
	AutoRetry          bool     `json:"autoRetry"`
	MaxRetry           int      `json:"maxRetry"`
	EnableLogs         bool     `json:"enableLogs"`
	LogLevel           string   `json:"logLevel"`
	Dirs               []string `json:"dirs"`
	RemoteServer       string   `json:"remoteServer"`
	RemoteUser         string   `json:"remoteUser"`
	RemotePass         string   `json:"remotePass"`
	LiveConfigPath     string   `json:"liveConfigPath"`
	RecorderContainer  string   `json:"recorderContainer"`
	RecorderConfigPath string   `json:"recorderConfigPath"`
	MailFrom           string   `json:"mailFrom"`
	MailAuthCode       string   `json:"mailAuthCode"`
	MailTo             string   `json:"mailTo"`
	EnableEncryption   bool     `json:"enableEncryption"`
	EnableUpload       bool     `json:"enableUpload"`
	ConvertMP4         bool     `json:"convertMP4"`
	WechatToken        string   `json:"wechatToken"`
	TelegramToken      string   `json:"telegramToken"`
	TelegramChatID     int64    `json:"telegramChatID"`
	QQBotWSURL         string   `json:"qqBotWsUrl"`
	QQBotToken         string   `json:"qqBotToken"`
	QQAdminID          int64    `json:"qqAdminId"`
	DashboardUser      string   `json:"dashboardUser"`
	DashboardPass      string   `json:"dashboardPass"`
}

// CLI 是启动参数快照，仅在 config.json 不存在时用于生成默认配置。
type CLI struct {
	Dirs               string
	Server             string
	Workers            int
	Rate               int
	DayRate            int
	NightRate          int
	ScanInterval       int
	ReportMinutes      int
	LiveConfigPath     string
	RecorderContainer  string
	RecorderConfigPath string
}

// Default 基于 CLI 参数生成首次启动配置（不预设真实远端口令）。
func Default(cli CLI) Config {
	dirs := []string{}
	for _, d := range strings.Split(cli.Dirs, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if cli.Workers <= 0 {
		cli.Workers = 3
	}
	if cli.ScanInterval <= 0 {
		cli.ScanInterval = 30
	}
	if cli.Server == "" {
		cli.Server = "http://127.0.0.1:5244"
	}
	return Config{
		ScanInterval:       cli.ScanInterval,
		Workers:            cli.Workers,
		Rate:               cli.Rate,
		DayRate:            cli.DayRate,
		NightRate:          cli.NightRate,
		EmailInterval:      cli.ReportMinutes,
		EnableLogs:         true,
		Dirs:               dirs,
		RemoteServer:       cli.Server,
		RemoteUser:         "admin",
		LiveConfigPath:     cli.LiveConfigPath,
		RecorderContainer:  cli.RecorderContainer,
		RecorderConfigPath: cli.RecorderConfigPath,
		EnableEncryption:   false,
		EnableUpload:       false,
		MailFrom:           "your_email@qq.com",
		MailAuthCode:       "your_auth_code",
		MailTo:             "receive_email@qq.com",
	}
}

// Load 读取 path；文件不存在返回 os.ErrNotExist。
func Load(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Workers <= 0 {
		c.Workers = 3
	}
	if c.ScanInterval <= 0 {
		c.ScanInterval = 30
	}
	if c.RemoteServer == "" {
		c.RemoteServer = "http://127.0.0.1:5244"
	}
}

// Validate 基本合法性检查。
func (c *Config) Validate() error {
	if c.Workers < 1 {
		return fmt.Errorf("workers 必须 >= 1")
	}
	if c.ScanInterval < 1 {
		return fmt.Errorf("scanInterval 必须 >= 1")
	}
	return nil
}

// Store 是线程安全的配置单例。
type Store struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

// NewStore 创建空配置仓库，path 为 config.json 路径。
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Get 返回配置副本（Dirs 切片复制，防外部改内部）。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cfg
	if s.cfg.Dirs != nil {
		out.Dirs = append([]string(nil), s.cfg.Dirs...)
	}
	return out
}

// Replace 整体替换配置（调用前应 Validate）。
func (s *Store) Replace(c Config) {
	c.applyDefaults()
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
}

// Update 在锁内执行 fn 修改配置。
func (s *Store) Update(fn func(*Config)) {
	s.mu.Lock()
	fn(&s.cfg)
	s.cfg.applyDefaults()
	s.mu.Unlock()
}

// Save 原子写回磁盘。
func (s *Store) Save() error {
	c := s.Get()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, data, 0644)
}

// LoadFromDisk 加载文件；不存在则用 cli 生成默认并落盘。
func (s *Store) LoadFromDisk(cli CLI) error {
	c, err := Load(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			// 解析失败也回落默认，避免起不来
			c = Default(cli)
		} else {
			c = Default(cli)
		}
	}
	s.Replace(c)
	return s.Save()
}
