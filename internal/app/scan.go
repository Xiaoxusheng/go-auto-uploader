package app

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"upload/internal/scanner"
	"upload/internal/storage"
)

// fileTask 扫描候选。
type fileTask struct {
	path string
	size int64
}

// RunOnce 单轮目录扫描 + 入队。返回仍在录制中的文件数。
func RunOnce(triggerReason string, currentDynamicInterval int) int {
	atomic.StoreInt64(&NextScanUnix, -1)

	if triggerReason == "start" || triggerReason == "rescan" {
		ResetFail()
	}

	currentWorkers := AppCfg().Workers
	cfg := AppCfg()
	currentDirs := make([]string, len(cfg.Dirs))
	copy(currentDirs, cfg.Dirs)
	enableUpload := cfg.EnableUpload

	if !IsRunning() {
		log.Println("[UPLOAD][PAUSED] 系统处于暂停状态，跳过本轮扫描")
		return 0
	}

	log.Printf("[SCAN][START] 🔍 启动目录探测，并发Workers:[%d] 目标路径:[%s]", currentWorkers, strings.Join(currentDirs, " | "))

	BroadcastWS("scanStarted", map[string]interface{}{
		"time":     time.Now().UnixMilli(),
		"dirs":     currentDirs,
		"interval": currentDynamicInterval,
		"workers":  currentWorkers,
		"trigger":  triggerReason,
	})

	for _, root := range currentDirs {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || root == "" {
			continue
		}
		if ds, exists := DirStatusStore.Get(root); exists {
			_ = ds
			ds.Mu.Lock()
			ds.PendingFiles = 0
			ds.TotalSize = 0
			ds.LastScanTime = time.Now().UnixMilli()
			ds.Mu.Unlock()
		} else {
			DirStatusStore.Put(root, &storage.DirStatus{
				Path:         root,
				LastScanTime: time.Now().UnixMilli(),
			})
		}
	}

	var newlyAddedFiles int32

	isQueuedFn := func(path string) bool { return TaskQueue.Contains(path) }
	if !enableUpload {
		isQueuedFn = func(string) bool { return true }
	}
	var scanErrCount int32
	scanRes := scanner.Scan(context.Background(), scanner.Options{
		Dirs:     currentDirs,
		IsQueued: isQueuedFn,
		OnFile: func(root, path string, size int64) {
			if ds, exists := DirStatusStore.Get(root); exists {
				_ = ds
				ds.Mu.Lock()
				ds.PendingFiles++
				ds.TotalSize += size
				ds.Mu.Unlock()
			}
		},
		OnZeroByte: func(path string) {
			log.Printf("[SCAN][CLEAN] 检测到遗留的 0 字节无效切片，已自动物理删除: %s", path)
		},
		OnError: func(path string, err error) {
			if err == nil || err == context.Canceled {
				return
			}
			n := atomic.AddInt32(&scanErrCount, 1)
			log.Printf("[SCAN][ERR] 访问路径出错 %s: %v", path, err)
			if n == 1 {
				SendAlert("warning", "目录扫描异常", "无法访问部分路径: "+err.Error())
				AddLog("error", "文件遍历失败", err.Error())
			}
		},
		IsRunning: IsRunning,
	})

	activeRecordingCount := scanRes.Active
	collectedTasks := make([]fileTask, 0, len(scanRes.Candidates))
	for _, c := range scanRes.Candidates {
		collectedTasks = append(collectedTasks, fileTask{path: c.Path, size: c.Size})
	}

	sort.Slice(collectedTasks, func(i, j int) bool {
		return collectedTasks[i].size > collectedTasks[j].size
	})

	if len(collectedTasks) > 1 {
		largest := collectedTasks[0]
		rest := collectedTasks[1:]
		var mixedTasks []fileTask
		left, right := 0, len(rest)-1
		for left <= right {
			mixedTasks = append(mixedTasks, rest[left])
			left++
			if left <= right {
				mixedTasks = append(mixedTasks, rest[right])
				right--
			}
		}
		collectedTasks = append(mixedTasks, largest)
	}

	for _, t := range collectedTasks {
		if TaskQueue.Enqueue(t.path) {
			atomic.AddInt64(&QueueCount, 1)
			atomic.AddInt32(&newlyAddedFiles, 1)
		}
	}

	DirStatusStore.Range(func(_ string, ds *storage.DirStatus) bool {
		ds.Mu.Lock()
		ds.TotalFiles = ds.PendingFiles + ds.UploadedFiles
		ds.Mu.Unlock()
		return true
	})
	MarkDirty()

	log.Printf("[SCAN][END] 🏁 本轮扫描完毕。发现新文件: %d 个 | 仍在录制中文件: %d 个", newlyAddedFiles, activeRecordingCount)

	BroadcastWS("scanFinished", map[string]interface{}{
		"time":  time.Now().UnixMilli(),
		"added": newlyAddedFiles,
	})

	return int(activeRecordingCount)
}
