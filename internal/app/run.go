package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"upload/internal/config"
	"upload/internal/logx"
	"upload/internal/naming"
	"upload/internal/ratelimit"
	"upload/internal/remote"
	"upload/internal/storage"
	"upload/internal/uploader"
)

// CLI 启动参数。
type CLI struct {
	Dirs               string
	Server             string
	Workers            int
	Rate               int
	DayRate            int
	NightRate          int
	ScanInterval       int
	ReportMinutes      int
	WebPort            int
	LiveConfigPath     string
	RecorderContainer  string
	RecorderConfigPath string
}

// Options Run 的注入项。
type Options struct {
	CLI      CLI
	StartWeb func(port int)
	InitBots func()
	// BuiltinActiveNames 返回内置引擎正在录制的主播名（清洗后）
	BuiltinActiveNames func() []string
	// FFmpegPath 返回 ffmpeg 路径
	FFmpegPath func() string
}

// SaveConfigToFile 保存配置并打日志。
func SaveConfigToFile() {
	if err := CfgStore.Save(); err != nil {
		log.Printf("[CONFIG][ERR] 无法保存配置文件 config.json: %v", err)
	}
}

// AddLog 投递业务日志。
func AddLog(level, message, errorMsg string) {
	if !AppCfg().EnableLogs {
		return
	}
	AppLogs.Add(level, message, errorMsg)
}

// Login 远端 OpenList 登录。
func Login() error {
	cfg := AppCfg()
	if RemoteCli == nil {
		RemoteCli = remote.NewOpenListClient(cfg.RemoteServer, cfg.RemoteUser, cfg.RemotePass, HTTPCli)
	} else {
		RemoteCli.SetCredentials(cfg.RemoteServer, cfg.RemoteUser, cfg.RemotePass)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return RemoteCli.Login(ctx)
}

// CurrentRate 当前限速：手动 Rate>0 时覆盖日夜表。
func CurrentRate() int {
	cfg := AppCfg()
	if cfg.Rate > 0 {
		return cfg.Rate
	}
	return ratelimit.Select(cfg.DayRate, cfg.NightRate, time.Now())
}

// BroadcastWS 广播到控制台。
func BroadcastWS(msgType string, payload interface{}) {
	if WSHub != nil {
		WSHub.PublishTyped(msgType, payload)
	}
}

// ensurePipeline 装配上传管线。
func ensurePipeline() *uploader.Pipeline {
	if Pipeline != nil {
		return Pipeline
	}
	ff := "ffmpeg"
	if FFmpegPathHook != nil {
		ff = FFmpegPathHook()
	}
	Pipeline = &uploader.Pipeline{
		SafeBaseDir: SafeBaseDir,
		ConvertMP4:  func() bool { return AppCfg().ConvertMP4 },
		FFmpegPath:  func() string { return ff },
		HashDB:      HashDB,
		History:     HistoryStore,
		Success:     SuccessStore,
		DirStatus:   DirStatusStore,
		MarkDirty:   MarkDirty,
		OnUpload: func(ctx context.Context, local, remotePath string, size int64) bool {
			return Upload(local, remotePath, size)
		},
		RecordSuccess: RecordSuccess,
		Broadcast:     BroadcastWS,
	}
	return Pipeline
}

// HandleFile 处理单个本地文件。
func HandleFile(path string) {
	ensurePipeline().HandleFile(context.Background(), path, AppCfg().Dirs)
}

// RecordSuccess 记录上传成功。
func RecordSuccess(remotePath, name string, size int64) {
	SuccessStore.Add(storage.UploadRecord{
		Time:     time.Now(),
		Streamer: naming.DetectStreamer(remotePath),
		Name:     name,
		Remote:   remotePath,
		Size:     size,
	})
}

// Run 启动完整应用生命周期（阻塞直到 SIGINT/SIGTERM）。
func Run(opts Options) {
	cli := opts.CLI

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(&logx.Interceptor{Original: os.Stdout, Store: AppLogs})

	if opts.BuiltinActiveNames != nil {
		BuiltinActiveNamesHook = opts.BuiltinActiveNames
	}
	if opts.FFmpegPath != nil {
		FFmpegPathHook = opts.FFmpegPath
	}

	InitHubs()

	if err := CfgStore.LoadFromDisk(config.CLI{
		Dirs:               cli.Dirs,
		Server:             cli.Server,
		Workers:            cli.Workers,
		Rate:               cli.Rate,
		DayRate:            cli.DayRate,
		NightRate:          cli.NightRate,
		ScanInterval:       cli.ScanInterval,
		ReportMinutes:      cli.ReportMinutes,
		LiveConfigPath:     cli.LiveConfigPath,
		RecorderContainer:  cli.RecorderContainer,
		RecorderConfigPath: cli.RecorderConfigPath,
	}); err != nil {
		log.Printf("[CONFIG][ERR] 加载配置失败: %v", err)
	}

	// 合并历史散落配置文件（builtin_config / builtin_cookies / bilibili_config）到 config.json
	if migrated, err := CfgStore.MigrateLegacy(); err != nil {
		log.Printf("[CONFIG][ERR] 合并历史配置文件失败: %v", err)
	} else if len(migrated) > 0 {
		log.Printf("[CONFIG] 🔄 已合并历史配置文件到 config.json: %v（原文件已备份为 .bak）", migrated)
	}

	if cli.Rate > 0 {
		CfgStore.Update(func(c *config.Config) { c.Rate = cli.Rate })
	}

	if len(AppCfg().Dirs) == 0 {
		log.Println("[CONFIG] ⚠️ 未配置扫描目录：可通过 -dirs 参数指定，或在 config.json / Web 控制台中配置 dirs 后再启动扫描")
	}

	// 运行时数据统一落到 config.dataDir，并迁移启动目录里的历史数据文件
	ApplyDataDir()
	SuccessStore.Load()
	HashDB.Load()
	DirStatusStore.Load()

	DashUser = "admin"
	DashPass = "admin"
	if AppCfg().DashboardUser != "" {
		DashUser = AppCfg().DashboardUser
	}
	if AppCfg().DashboardPass != "" {
		DashPass = AppCfg().DashboardPass
	}
	if DashUser == "admin" && DashPass == "admin" {
		log.Println("[AUTH] 🚨 高危告警：控制台仍在使用默认弱口令 admin/admin，请立即在 config.json 中配置 dashboardUser 与 dashboardPass！")
	}

	SaveConfigToFile()
	AddLog("info", "系统初始化完成，启动中...", "")

	if opts.StartWeb != nil {
		go opts.StartWeb(cli.WebPort)
	}
	go queueStatusLoop()
	go reportLoop()
	go dirStatusPersistLoop()
	go successLogPersistLoop()
	go manageWorkers()
	if opts.InitBots != nil {
		opts.InitBots()
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("[SYSTEM] 收到退出信号 %v，正在优雅停机…", sig)
		SetRunning(false)
		if AppCancel != nil {
			AppCancel()
		}
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()

	baseInterval := AppCfg().ScanInterval
	currentDynamicInterval := baseInterval
	atomic.StoreInt64(&DynInterval, int64(currentDynamicInterval))

	if IsRunning() {
		_ = Login()
		activeCount := RunOnce("auto", currentDynamicInterval)
		currentDynamicInterval = adjustInterval(baseInterval, activeCount, currentDynamicInterval)
	}

	for {
		if IsRunning() {
			targetTime := time.Now().Add(time.Duration(currentDynamicInterval) * time.Minute)
			atomic.StoreInt64(&NextScanUnix, targetTime.UnixMilli())

			select {
			case <-time.After(time.Until(targetTime)):
				_ = Login()
				baseInterval = AppCfg().ScanInterval
				activeCount := RunOnce("auto", currentDynamicInterval)
				currentDynamicInterval = adjustInterval(baseInterval, activeCount, currentDynamicInterval)
			case reason := <-TriggerScanCh:
				_ = Login()
				log.Printf("[SYSTEM] ⚡ 收到指令打断，执行扫描 (触发源: %s)", reason)
				baseInterval = AppCfg().ScanInterval
				activeCount := RunOnce(reason, currentDynamicInterval)
				currentDynamicInterval = adjustInterval(baseInterval, activeCount, currentDynamicInterval)
			}
		} else {
			atomic.StoreInt64(&NextScanUnix, 0)
			select {
			case <-time.After(5 * time.Second):
			case <-TriggerScanCh:
				log.Println("[SYSTEM] ⚡ 系统重新启动或热重载完成")
			}
		}
	}
}

func adjustInterval(base, activeCount, current int) int {
	if activeCount > 0 {
		newInterval := base / (activeCount + 1)
		if newInterval < 3 {
			newInterval = 3
		}
		if newInterval != current {
			log.Printf("[SCAN][DYNAMIC] 🔥 当前有 %d 个主播处于录制写入状态，按比例调整下次扫描为 %d 分钟后", activeCount, newInterval)
		}
		atomic.StoreInt64(&DynInterval, int64(newInterval))
		return newInterval
	}
	if current != base {
		log.Printf("[SCAN][DYNAMIC] 🟢 当前暂无录制任务，扫描间隔回归省电模式: %d 分钟", base)
	}
	atomic.StoreInt64(&DynInterval, int64(base))
	return base
}

func manageWorkers() {
	AppCtx, AppCancel = context.WithCancel(context.Background())
	UploadPool = uploader.NewWorkerPool(TaskQueue, func(ctx context.Context, path string) {
		atomic.AddInt64(&ActiveWorker, 1)
		defer atomic.AddInt64(&ActiveWorker, -1)
		HandleFile(path)
	}, func() bool {
		return !IsRunning()
	})
	UploadPool.Start(AppCtx, func() int { return AppCfg().Workers })

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-AppCtx.Done():
				return
			case <-t.C:
				atomic.StoreInt64(&QueueCount, TaskQueue.Pending())
			}
		}
	}()
}

func queueStatusLoop() {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	for range ticker.C {
		log.Printf("[QUEUE][STATUS] 运行时长:%s | 等待任务:%d | 活动Worker:%d | 并发设定:%d",
			time.Since(start).Truncate(time.Second),
			atomic.LoadInt64(&QueueCount),
			atomic.LoadInt64(&ActiveWorker),
			AppCfg().Workers,
		)
	}
}

func dirStatusPersistLoop() {
	stop := make(chan struct{})
	if AppCtx != nil {
		go func() { <-AppCtx.Done(); close(stop) }()
	}
	DirStatusStore.PersistLoop(5*time.Second, stop)
}

func successLogPersistLoop() {
	stop := make(chan struct{})
	if AppCtx != nil {
		go func() { <-AppCtx.Done(); close(stop) }()
	}
	SuccessStore.PersistLoop(15*time.Second, stop)
}

// PauseOnFailure 熔断挂起。
func PauseOnFailure(reason string) {
	wasRunning := IsRunning()
	SetRunning(false)

	if wasRunning {
		log.Printf("[SYSTEM][AUTO-PAUSE] 🚨 触发安全熔断机制: %s", reason)
		SendAlert("error", "🛑 触发系统熔断保护", reason+"\n为防止本地任务大量报错，上传引擎已自动挂起！请检查远端服务器状态后，手动点击【启动扫描】恢复运行。")
		SendWeChatNotify("异常通知", fmt.Sprintf("系统已触发自动保护熔断并挂起队列上传。异常原因：\n%s", reason))
	}
}

// DetectRoot 根目录反推。
func DetectRoot(path string) string {
	return naming.DetectRoot(path, AppCfg().Dirs)
}

// ApplyDataDir 解析 dataDir、迁移历史数据文件，并把各持久化 store 重定向过去。
//
// 关键：只 Repath，绝不重建实例。internal/bots 在包 init 阶段就捕获了
// SuccessStore 的引用，若这里替换全局指针，bots 会拿到一个永不加载、
// 路径也错的僵尸 store（图表/趋势数据全空，且会往错误位置写文件）。
// 返回生效的数据目录。
func ApplyDataDir() string {
	dataDir := setupDataDir()
	SuccessStore.Repath(filepath.Join(dataDir, successLogName))
	DirStatusStore.Repath(filepath.Join(dataDir, dirStatusName))
	HashDB.Repath(filepath.Join(dataDir, hashName))
	return dataDir
}

// setupDataDir 解析并创建运行时数据目录（config.dataDir），并把历史散落数据文件搬进去。
func setupDataDir() string {	dir := AppCfg().DataDirPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[CONFIG] ⚠️ 无法创建数据目录 %s: %v，回退到当前目录", dir, err)
		return "."
	}
	migrateLegacyDataFiles(dir)
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	log.Printf("[CONFIG] 📁 运行时数据目录: %s", abs)
	return dir
}

// migrateLegacyDataFiles 把启动目录里的历史数据文件搬进 dataDir（仅当目标不存在且源存在）。
// 不迁移会导致换目录后 hash 库为空、已上传文件被全部重传。
func migrateLegacyDataFiles(dataDir string) {
	if filepath.Clean(dataDir) == "." {
		return
	}
	for _, name := range []string{hashName, successLogName, dirStatusName} {
		dst := filepath.Join(dataDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // 目标已存在，保留
		}
		if _, err := os.Stat(name); err != nil {
			continue // 源不存在
		}
		if err := moveFile(name, dst); err != nil {
			log.Printf("[CONFIG] ⚠️ 迁移数据文件 %s → %s 失败: %v", name, dst, err)
			continue
		}
		log.Printf("[CONFIG] 🔄 已迁移数据文件 %s → %s", name, dst)
	}
}

// moveFile 优先 rename，跨盘时回退为复制+删除。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
