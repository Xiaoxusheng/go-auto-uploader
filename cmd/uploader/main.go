package main

import (
	"flag"
	"log"
	"strings"

	"upload/internal/app"
	"upload/internal/bots"
	"upload/internal/recorder"
)

func builtinActiveNames() []string {
	var names []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		names = append(names, s)
	}
	for _, t := range recorder.Tasks() {
		if !recorder.IsLiveStatus(t.Status) {
			continue
		}
		safe := t.AnchorName
		for _, c := range []string{`\`, `/`, `:`, `*`, `?`, `"`, `<`, `>`, `|`, "\r", "\n", "\t", "　"} {
			safe = strings.ReplaceAll(safe, c, "")
		}
		safe = strings.Trim(strings.TrimSpace(safe), " ._-")
		if safe == "" {
			safe = t.RoomID
		}
		add(safe)
		add(t.AnchorName)
	}
	return names
}

func main() {
	var cli app.CLI

	flag.StringVar(&cli.Dirs, "dirs", "", "扫描目录(逗号分隔)")
	flag.StringVar(&cli.Server, "server", "http://127.0.0.1:5244", "服务器")
	flag.IntVar(&cli.Workers, "workers", 3, "并发")
	flag.IntVar(&cli.Rate, "rate", 0, "手动限速 MB/s（>0 时覆盖日夜限速）")
	flag.IntVar(&cli.DayRate, "day-rate", 20, "白天限速 MB/s")
	flag.IntVar(&cli.NightRate, "night-rate", 80, "夜晚限速 MB/s")
	flag.IntVar(&cli.ScanInterval, "scan-interval", 30, "默认30min扫描一次")
	flag.IntVar(&cli.ReportMinutes, "report-minutes", 360, "邮件统计分钟")
	flag.IntVar(&cli.WebPort, "web-port", 8080, "Web API 端口")
	flag.StringVar(&cli.LiveConfigPath, "live-config", "/home/live/DouyinLiveRecorder/config/URL_config.ini", "录制配置文件路径")
	flag.StringVar(&cli.RecorderContainer, "recorder-container", "douyinliverecorder-app-1", "录制引擎Docker容器名")
	flag.StringVar(&cli.RecorderConfigPath, "recorder-config", "", "录制引擎主配置文件(config.ini)路径")
	flag.Parse()

	app.Run(app.Options{
		CLI:                cli,
		StartWeb:           startWebServer,
		InitBots:           initBots,
		BuiltinActiveNames: builtinActiveNames,
		FFmpegPath:         recorder.FFmpegBin,
	})
}

func initBots() {
	go bots.InitTelegram()
	go bots.InitQQ()
}

func startWebServer(port int) {
	if err := startHTTP(port); err != nil {
		log.Fatalf("[WEB] Server error: %v", err)
	}
}
