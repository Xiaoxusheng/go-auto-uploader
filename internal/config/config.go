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
// 原本散落的 builtin_config.json / builtin_cookies.json / bilibili_config.json
// 已合并为 builtin / bilibili 两个子对象，统一落在同一个 config.json 内。
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
	MailSMTPHost       string   `json:"mailSmtpHost"`
	MailSMTPPort       int      `json:"mailSmtpPort"`
	EnableEncryption   bool     `json:"enableEncryption"`
	EnableUpload       bool     `json:"enableUpload"`
	ConvertMP4         bool     `json:"convertMP4"`
	WechatToken        string   `json:"wechatToken"`
	TelegramToken      string   `json:"telegramToken"`
	TelegramChatID     int64    `json:"telegramChatID"`
	DashboardUser      string   `json:"dashboardUser"`
	DashboardPass      string   `json:"dashboardPass"`

	// DataDir 运行时数据目录（hash 库 / 成功日志 / 目录状态），默认 ./data
	DataDir string `json:"dataDir"`

	// Builtin 内置录制引擎配置（原 builtin_config.json + builtin_cookies.json）
	Builtin BuiltinSettings `json:"builtin"`

	// Bilibili 预留：原 bilibili_config.json（当前代码未消费，仅保留数据不丢）
	Bilibili BilibiliSettings `json:"bilibili"`
}

// BuiltinSettings 内置录制引擎参数（字段名与原 builtin_config.json 完全兼容）。
type BuiltinSettings struct {
	Quality              string `json:"quality"`
	SegmentTime          int    `json:"segment_time"`
	CheckInterval        int    `json:"check_interval"`
	SavePath             string `json:"save_path"`
	WatermarkEnable      bool   `json:"watermark_enable"`       // 截图水印
	VideoWatermarkEnable bool   `json:"video_watermark_enable"` // 视频烧录水印（需重编码）
	WatermarkText        string `json:"watermark_text"`
	WatermarkFormat      string `json:"watermark_format"`
	WatermarkPosition    string `json:"watermark_position"`
	WatermarkFontSize    int    `json:"watermark_font_size"`
	WatermarkFontColor   string `json:"watermark_font_color"`

	// Cookies 原 builtin_cookies.json
	Cookies BuiltinCookies `json:"cookies"`
}

// BuiltinCookies 多平台防爬虫鉴权会话（字段名与原 builtin_cookies.json 兼容）。
type BuiltinCookies struct {
	Douyin   string `json:"douyin"`
	Kuaishou string `json:"kuaishou"`
	Soop     string `json:"soop"`
	Bilibili string `json:"bilibili"`
	Twitch   string `json:"twitch"`
}

// BilibiliSettings 原 bilibili_config.json 的完整字段（保持兼容，暂未消费）。
type BilibiliSettings struct {
	Enable        bool   `json:"enable"`
	SessData      string `json:"sessdata"`
	BiliJct       string `json:"bili_jct"`
	DedeUserID    string `json:"dedeuserid"`
	Tid           int    `json:"tid"`
	Tag           string `json:"tag"`
	TitleTemplate string `json:"titleTemplate"`
	Desc          string `json:"desc"`
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
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.MailSMTPHost == "" {
		c.MailSMTPHost = "smtp.qq.com"
	}
	if c.MailSMTPPort <= 0 {
		c.MailSMTPPort = 587
	}
	c.Builtin.ApplyDefaults()
}

// ApplyDefaults 补足内置引擎配置缺省值（兼容旧文件缺失字段）。
func (b *BuiltinSettings) ApplyDefaults() {
	if b.Quality == "" {
		b.Quality = "uhd"
	}
	if b.CheckInterval == 0 {
		b.CheckInterval = 30
	}
	if b.SavePath == "" {
		b.SavePath = "./downloads"
	}
	if b.WatermarkFormat == "" {
		b.WatermarkFormat = "%Y-%m-%d %H:%M:%S"
	}
	if b.WatermarkPosition == "" {
		b.WatermarkPosition = "bottom-right"
	}
	if b.WatermarkFontSize == 0 {
		b.WatermarkFontSize = 38
	}
	if b.WatermarkFontColor == "" {
		b.WatermarkFontColor = "white@0.95"
	}
}

// DataDirPath 返回生效的数据目录（已补默认值）。
func (c Config) DataDirPath() string {
	if c.DataDir == "" {
		return "./data"
	}
	return c.DataDir
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

// MigrateLegacy 把历史散落配置文件合并进当前配置并落盘，返回被迁移的文件名。
// 仅当历史文件存在且能被正确解析时才合并并改名 .bak，解析失败时原文件保持不动。
func (s *Store) MigrateLegacy() ([]string, error) {
	s.mu.Lock()
	migrated := s.cfg.MigrateLegacy()
	s.mu.Unlock()
	if len(migrated) == 0 {
		return nil, nil
	}
	return migrated, s.Save()
}

// MigrateLegacy 就地合并历史配置文件（builtin_config / builtin_cookies / bilibili_config）。
func (c *Config) MigrateLegacy() []string {
	var migrated []string

	if data, err := os.ReadFile("builtin_config.json"); err == nil {
		var b BuiltinSettings
		if json.Unmarshal(data, &b) == nil {
			// 保留已合并进来的 cookies，避免被覆盖成空
			prevCookies := c.Builtin.Cookies
			c.Builtin = b
			c.Builtin.Cookies = prevCookies
			migrated = append(migrated, "builtin_config.json")
			backupLegacy("builtin_config.json")
		}
	}

	if data, err := os.ReadFile("builtin_cookies.json"); err == nil {
		var ck BuiltinCookies
		if json.Unmarshal(data, &ck) == nil {
			c.Builtin.Cookies = ck
			migrated = append(migrated, "builtin_cookies.json")
			backupLegacy("builtin_cookies.json")
		}
	}

	if data, err := os.ReadFile("bilibili_config.json"); err == nil {
		var bi BilibiliSettings
		if json.Unmarshal(data, &bi) == nil {
			c.Bilibili = bi
			migrated = append(migrated, "bilibili_config.json")
			backupLegacy("bilibili_config.json")
		}
	}

	c.applyDefaults()
	return migrated
}

// backupLegacy 将已合并的历史文件改名为 .bak（覆盖旧备份）。
func backupLegacy(path string) {
	_ = os.Remove(path + ".bak")
	_ = os.Rename(path, path+".bak")
}
