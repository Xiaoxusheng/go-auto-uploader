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
	ScreenshotInterval   int    `json:"screenshot_interval"` // 定期截图间隔（秒），0=默认20
	SavePath             string `json:"save_path"`
	WatermarkEnable      bool   `json:"watermark_enable"`       // 截图水印
	VideoWatermarkEnable bool   `json:"video_watermark_enable"` // 视频烧录水印（需重编码）
	WatermarkText        string `json:"watermark_text"`
	WatermarkFormat      string `json:"watermark_format"`
	WatermarkPosition    string `json:"watermark_position"`
	WatermarkFontSize    int    `json:"watermark_font_size"`
	WatermarkFontColor   string `json:"watermark_font_color"`

	// Highlight* 高光切片：录制切片落盘后离线分析，自动裁出活跃片段。
	// 判定用「画面运动量 + 音频能量」双因子，权重默认偏向运动量——
	// 实测跳舞直播里音频与「跳舞」负相关（主播跳舞时停止说话）。
	HighlightEnable    bool    `json:"highlight_enable"`        // 总开关，默认关闭
	HighlightMotionW   float64 `json:"highlight_motion_weight"` // 画面运动量权重
	HighlightAudioW    float64 `json:"highlight_audio_weight"`  // 音频能量权重
	HighlightThreshold float64 `json:"highlight_threshold"`     // 综合分阈值（自适应 z 分，非绝对量）
	HighlightMinDur    int     `json:"highlight_min_duration"`  // 最短高光（秒），短于此丢弃
	HighlightMaxDur    int     `json:"highlight_max_duration"`  // 单个高光最长（秒）
	HighlightPerClip   int     `json:"highlight_per_clip"`      // 每个切片最多产出几个高光
	HighlightMergeGap  int     `json:"highlight_merge_gap"`     // 相邻候选段合并间隔（秒）
	// HighlightSmoothWindow z 分归一化前的滑动平均窗口（秒），0/未配置时回落 5。
	// 调大它会让运动量曲线更平滑，压掉「礼物特效/切场景」那种几秒的孤立尖峰；
	// ⚠️ 但它同时会缩小 MAD、把 z 分整体放大，所以**必须与 HighlightThreshold 一起改**：
	// 实测把 5→15 时，阈值要 1.2→1.8 才对应同一档灵敏度（见 docs/highlight-progress.md §13）。
	HighlightSmoothWindow int `json:"highlight_smooth_window"`
	// HighlightOnlyUpload 为 true 时，开了高光的主播只上传高光片段，原片保留在本地不上传。
	// 单主播可用「只传高光:1/0」覆盖。
	HighlightOnlyUpload bool `json:"highlight_only_upload"`
	// HighlightSourceRetentionDays 源片最长保留天数，0 或负值表示关闭。
	//
	// 正常路径下源片会在「上传成功」或「高光分析完成」后立即删除，这个兜底只针对
	// 异常残留：上传长期失败、分析反复失败、标了「只传高光」但高光从未跑起来等。
	// 没有它，这类文件会一直占盘直到写满（线上实测单个主播堆到 14GB）。
	//
	// 用指针是为了区分「未配置」（nil → 默认 7 天）与「显式关闭」（0）。
	// 未开启上传的实例（采集/训练机）不执行清理 —— 那种实例本地就是唯一副本。
	HighlightSourceRetentionDays *int `json:"highlight_source_retention_days"`

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
	if b.ScreenshotInterval <= 0 {
		b.ScreenshotInterval = 20
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
	// 高光参数：基于真实素材校准的经验值。
	// 权重两者同时为 0 才视为「未设置」，这样用户可把音频权重显式设为 0、只保留运动量。
	if b.HighlightMotionW == 0 && b.HighlightAudioW == 0 {
		b.HighlightMotionW, b.HighlightAudioW = 0.8, 0.2
	}
	if b.HighlightThreshold <= 0 {
		b.HighlightThreshold = 1.5
	}
	// 平滑窗口默认 5，与改动前 score.go 的常量一致 —— 未配置时行为完全不变。
	if b.HighlightSmoothWindow <= 0 {
		b.HighlightSmoothWindow = 5
	}
	if b.HighlightMinDur <= 0 {
		b.HighlightMinDur = 15
	}
	if b.HighlightMaxDur <= 0 {
		b.HighlightMaxDur = 180
	}
	if b.HighlightPerClip <= 0 {
		b.HighlightPerClip = 3
	}
	if b.HighlightMergeGap <= 0 {
		// 鲁棒性关键参数：太小会在候选间隔处断链，把一整支舞切成互不相连的几段。
		b.HighlightMergeGap = 20
	}
}

// defaultSourceRetentionDays 是源片兜底清理的默认保留天数。
const defaultSourceRetentionDays = 7

// SourceRetentionDays 返回生效的源片保留天数。
//
// 未配置时返回默认的 7 天；显式配置 0 或负值表示关闭清理（返回 0）。
// 之所以用指针字段 + 取值方法、而不是在 applyDefaults 里补默认值，
// 是为了让「没配过」和「明确要关掉」这两种情况区分得开。
func (b BuiltinSettings) SourceRetentionDays() int {
	if b.HighlightSourceRetentionDays == nil {
		return defaultSourceRetentionDays
	}
	if *b.HighlightSourceRetentionDays <= 0 {
		return 0
	}
	return *b.HighlightSourceRetentionDays
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
