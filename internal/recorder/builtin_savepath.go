// Package recorder — 存储溢出护栏：主落盘目录剩余空间跌破阈值时，新录制自动切到
// 白名单备选目录里剩余空间最大且达标的那个；主目录恢复后自动切回（主目录优先，
// 天然无抖动）。备选目录全部不足时维持主目录并告警——绝不自动扫盘乱找。
// 设计与全链路约束见 docs/plans/2026-09-30-storage-fallback.md。
package recorder

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"upload/internal/fsutil"
)

// AlertHook 存储护栏告警出口，由 app 注入 SendAlert（recorder 不可反向 import app）。
// 为 nil 时只记运行日志。
var AlertHook func(level, title, message string)

// freeSpaceFn 磁盘剩余空间探测（字节），测试可注入替换。
var freeSpaceFn = fsutil.FreeSpace

// storageAlertCooldown 同类存储告警的节流间隔。
const storageAlertCooldown = 30 * time.Minute

var (
	storageAlertMu   sync.Mutex
	storageAlertLast = map[string]time.Time{}
)

// FallbackRoots 返回归一化后的备选落盘根目录（Clean/去空/去重/剔除与主目录重复项）。
// 配置层 ApplyDefaults 已归一化一次，这里兜底运行时直接改内存配置的场景。
func FallbackRoots() []string {
	c := Config()
	primary := filepath.Clean(getBuiltinSavePath())
	seen := map[string]bool{primary: true}
	out := make([]string, 0, len(c.SavePathFallbacks))
	for _, p := range c.SavePathFallbacks {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// RecordRoots 返回主目录 + 全部备选目录（录制产物可能存在的所有根目录）。
// 供高光分析、统计汇总等需要跨根遍历的冷路径使用。
func RecordRoots() []string {
	return append([]string{getBuiltinSavePath()}, FallbackRoots()...)
}

// ResolveRecordRoot 为一次新录制选择落盘根目录：
//  1. 未配置备选目录 → 功能关闭，返回主目录；
//  2. 主目录剩余 ≥ 阈值 → 主目录；
//  3. 备选中剩余 ≥ 阈值且最大者 → 该备选（首次切换时建目录 + 告警）；
//  4. 备选全部不足 → 维持主目录并告警（节流），不乱找。
func ResolveRecordRoot() string {
	primary := getBuiltinSavePath()
	fallbacks := FallbackRoots()
	if len(fallbacks) == 0 {
		return primary
	}
	minFree := int64(Config().MinFreeGB * 1073741824)
	if freeSpaceFn(primary) >= minFree {
		return primary
	}
	best, bestFree := "", int64(-1)
	for _, fb := range fallbacks {
		if f := freeSpaceFn(fb); f > bestFree {
			best, bestFree = fb, f
		}
	}
	if best != "" && bestFree >= minFree {
		if err := os.MkdirAll(best, os.ModePerm); err != nil {
			log.Printf("[BUILTIN][STORAGE][ERR] 备选目录不可创建 %s: %v，维持主目录", best, err)
			return primary
		}
		alertStorage("switch", "warning", "录制存储已切换备选目录",
			fmt.Sprintf("主目录 %s 剩余 %.1fGB（阈值 %.0fGB），新录制自动切换到备选目录 %s（剩余 %.1fGB）。",
				primary, gb(freeSpaceFn(primary)), Config().MinFreeGB, best, gb(bestFree)))
		return best
	}
	alertStorage("overflow", "error", "录制存储空间告急",
		fmt.Sprintf("主目录 %s 剩余 %.1fGB，备选目录均低于阈值 %.0fGB，新录制只能继续写主目录，请尽快清理磁盘。",
			primary, gb(freeSpaceFn(primary)), Config().MinFreeGB))
	return primary
}

// alertStorage 记日志并经 AlertHook 下发控制台告警，同一 reason 节流。
func alertStorage(reason, level, title, message string) {
	log.Printf("[BUILTIN][STORAGE] %s: %s", title, message)
	if AlertHook == nil {
		return
	}
	storageAlertMu.Lock()
	defer storageAlertMu.Unlock()
	if t, ok := storageAlertLast[reason]; ok && time.Since(t) < storageAlertCooldown {
		return
	}
	storageAlertLast[reason] = time.Now()
	AlertHook(level, title, message)
}

func gb(bytes int64) float64 {
	return float64(bytes) / 1073741824
}
