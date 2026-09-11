package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	lastActiveMap   = make(map[string]bool)
	lastActiveMapMu sync.Mutex
)

// GetActiveStreamers 探测各目录近期写入，并对比推送开播/下播通知。
func GetActiveStreamers() []string {
	configuredDirs := AppCfg().Dirs
	activeMap := make(map[string]bool)

	for _, dir := range configuredDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if time.Since(info.ModTime()) < 3*time.Minute {
				rel, err := filepath.Rel(dir, path)
				if err == nil {
					parts := strings.Split(filepath.ToSlash(rel), "/")
					if len(parts) >= 2 {
						activeMap[parts[len(parts)-2]] = true
					} else if len(parts) == 1 {
						activeMap[strings.Split(parts[0], "_")[0]] = true
					}
				}
			}
			return nil
		})
	}

	var result []string
	for k := range activeMap {
		result = append(result, k)
	}

	builtinNames := make(map[string]bool)
	if BuiltinActiveNamesHook != nil {
		for _, name := range BuiltinActiveNamesHook() {
			if name != "" {
				builtinNames[name] = true
			}
		}
	}

	lastActiveMapMu.Lock()
	for streamer := range activeMap {
		if !lastActiveMap[streamer] && !builtinNames[streamer] {
			SendWeChatNotify("开播通知", fmt.Sprintf("检测到外部录制引擎中主播 [%s] 的文件夹有新数据写入，判断为开始录制！", streamer))
		}
	}
	for streamer := range lastActiveMap {
		if !activeMap[streamer] && !builtinNames[streamer] {
			SendWeChatNotify("下播通知", fmt.Sprintf("检测到外部录制引擎中主播 [%s] 的文件夹已停止数据写入，判断为结束录制！", streamer))
		}
	}
	lastActiveMap = activeMap
	lastActiveMapMu.Unlock()

	return result
}
