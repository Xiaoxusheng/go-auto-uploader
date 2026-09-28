// cleanup-sources 磁盘自动治理：删除「训练数据已用完」的源片。
//
// 用完 = 该片已入池（clips_config 有条目 = 1fps 帧 + 每秒姿态特征均已落盘，
// 训练/复核/重定标只用帧与特征，源片不再被任何环节引用）。删除后源片不可恢复，
// 但训练产物完整；若未来需要 5fps 重抽/音频特征，仅对仍在保护目录中的片可行。
//
// 安全规则（全部满足才删）：
//   - 仅 downloads 根下的 .ts；
//   - 片名在池配置中；
//   - 帧目录仍存在（训练数据完好，源片才是纯冗余；帧目录丢了说明状态异常，跳过待人工）；
//   - 文件 mtime 距今 ≥ -min-age-hours（保护仍在写入的录制）；
//   - 片名命中 -protect-dirs 中的文件则跳过（冻结集/探针源的下载副本，宁保守）；
//   - 全部删除动作追加写入审计日志。
//
// 用法：
//
//	hleval cleanup-sources [-downloads <dir>] [-config <json>] [-frames <dir>]
//	                     [-protect-dirs <dir1,dir2>] [-min-age-hours 1]
//	                     [-log <audit.log>] [-dry-run]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func cmdCleanupSources(args []string) {
	fs := flag.NewFlagSet("cleanup-sources", flag.ExitOnError)
	downloads := fs.String("downloads", "D:/upload/downloads", "源片根目录")
	cfgPath := fs.String("config", "D:/upload/_diag/train/_pose_pilot/clips_config.json", "池配置 JSON")
	framesRoot := fs.String("frames", "D:/upload/_diag/train/_pose_pilot/frames", "帧根目录（存在性检查）")
	protectDirs := fs.String("protect-dirs", "D:/upload/_diag/train/freeze_v2_sources,D:/upload/_diag/train/audio_probe/sources", "保护目录（命中片名一律跳过）")
	minAge := fs.Float64("min-age-hours", 1, "文件最小年龄（小时），保护写入中的录制")
	logPath := fs.String("log", "D:/upload/_diag/train/_deleted_ingested_sources.log", "审计日志（追加）")
	dryRun := fs.Bool("dry-run", false, "只报告不删除")
	fs.Parse(args)

	cfg := []clipConfigEntry{}
	if b, err := os.ReadFile(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "读池配置失败: %v\n", err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "解析池配置失败: %v\n", err)
		os.Exit(1)
	}
	pool := make(map[string]bool, len(cfg))
	for _, e := range cfg {
		pool[e.Clip] = true
	}
	protect := map[string]bool{}
	for _, d := range strings.Split(*protectDirs, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		for _, f := range mustGlob(d) {
			protect[strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))] = true
		}
	}

	// downloads 按主播/<日期>/ 分层，必须递归遍历（曾用顶层 glob 致扫描恒为空）
	var files []string
	_ = filepath.WalkDir(*downloads, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".ts") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)

	now := time.Now()
	var deleted []string
	var freedBytes int64
	keptPool, keptAge, keptNoFrame, keptProtect := 0, 0, 0, 0
	for _, f := range files {
		stem := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		switch {
		case !pool[stem]:
			keptPool++ // 未入池：训练还没用过它
			continue
		case protect[stem]:
			keptProtect++ // 保护集合（冻结/探针源），宁保守
			continue
		}
		// 注意：这里传的是 glob 模式，必须直接 Glob——mustGlob 是「列目录」语义，
		// 会把整个模式当路径 Stat，恒失败（曾致全部窗口判无帧）
		if fs2, _ := filepath.Glob(filepath.Join(*framesRoot, stem, "f_*.jpg")); len(fs2) < ueMinFrames {
			keptNoFrame++ // 帧目录缺失=状态异常，跳过待人工
			continue
		}
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()).Hours() < *minAge {
			keptAge++ // 仍在写入的录制
			continue
		}
		if *dryRun {
			deleted = append(deleted, fmt.Sprintf("[dry-run] %s\t%d", f, fi.Size()))
			freedBytes += fi.Size()
			continue
		}
		if err := os.Remove(f); err != nil {
			fmt.Fprintf(os.Stderr, "删除失败 %s: %v\n", f, err)
			continue
		}
		deleted = append(deleted, fmt.Sprintf("%s\t%d", f, fi.Size()))
		freedBytes += fi.Size()
	}

	summary := fmt.Sprintf("cleanup-sources: 删除 %d 个源片，回收 %.1fGB（保留：未入池 %d / 保护 %d / 无帧 %d / 新写 %d）",
		len(deleted), float64(freedBytes)/1e9, keptPool, keptProtect, keptNoFrame, keptAge)
	fmt.Println(summary)
	if *dryRun {
		fmt.Println("(dry-run，未实际删除)")
		return
	}
	if len(deleted) > 0 && *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "# %s 删除 %d 个 / %.1fGB\n%s\n",
				now.Format("2006-01-02 15:04:05"), len(deleted), float64(freedBytes)/1e9,
				strings.Join(deleted, "\n"))
			_ = f.Close()
		}
	}
}

// mustGlob 目录 glob（目录不存在返回空，不报错）。
func mustGlob(dir string) []string {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil
	}
	f, _ := filepath.Glob(filepath.Join(dir, "*"))
	return f
}
