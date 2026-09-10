// Package scanner 负责发现本地待上传文件候选，不执行上传、不触达远端。
package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Candidate 是扫描发现的可上传文件。
type Candidate struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Options 控制扫描行为与副作用回调。回调在 Walk 协程内同步调用，需自行保证线程安全。
type Options struct {
	Dirs []string
	// SkipFreshFor 该时长内修改过的文件视为仍在写入（录制中），不入候选。默认 2 分钟。
	SkipFreshFor time.Duration
	// IsQueued 若路径已在队列/处理中，仍计入目录统计但不产出 Candidate。
	IsQueued func(path string) bool
	// OnFile 每发现一个「非录制中、非 0 字节」的合法文件时回调（含已在队列的）。
	OnFile func(root, path string, size int64)
	// OnActive 仍在写入的文件。
	OnActive func(path string)
	// OnZeroByte 已物理删除的 0 字节文件。
	OnZeroByte func(path string)
	// OnArtifact 跳过的 .part/.tmp。
	OnArtifact func(path string)
	// OnError Walk 错误（路径不可访问等），path 可能为空。
	OnError func(path string, err error)
	// IsRunning 每次访问文件前检查；返回 false 则中止本轮扫描。
	IsRunning func() bool
}

// Result 汇总本轮扫描。
type Result struct {
	Active     int32
	Candidates []Candidate
}

// Scan 并发遍历 opts.Dirs，返回候选列表与活跃写入文件数。
func Scan(ctx context.Context, opts Options) Result {
	if opts.SkipFreshFor <= 0 {
		opts.SkipFreshFor = 2 * time.Minute
	}

	var res Result
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, root := range opts.Dirs {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || root == "" {
			continue
		}
		wg.Add(1)
		go func(scanRoot string) {
			defer wg.Done()
			_ = filepath.WalkDir(scanRoot, func(path string, d os.DirEntry, err error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if opts.IsRunning != nil && !opts.IsRunning() {
					return context.Canceled
				}
				if err != nil {
					if opts.OnError != nil {
						opts.OnError(path, err)
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}

				lower := strings.ToLower(d.Name())
				if strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".tmp") {
					if opts.OnArtifact != nil {
						opts.OnArtifact(path)
					}
					return nil
				}

				info, err := d.Info()
				if err != nil {
					if opts.OnError != nil {
						opts.OnError(path, err)
					}
					return nil
				}

				if time.Since(info.ModTime()) < opts.SkipFreshFor {
					atomic.AddInt32(&res.Active, 1)
					if opts.OnActive != nil {
						opts.OnActive(path)
					}
					return nil
				}

				if info.Size() == 0 {
					_ = os.Remove(path)
					if opts.OnZeroByte != nil {
						opts.OnZeroByte(path)
					}
					return nil
				}

				if opts.OnFile != nil {
					opts.OnFile(scanRoot, path, info.Size())
				}

				if opts.IsQueued != nil && opts.IsQueued(path) {
					return nil
				}

				mu.Lock()
				res.Candidates = append(res.Candidates, Candidate{
					Path:    path,
					Size:    info.Size(),
					ModTime: info.ModTime(),
				})
				mu.Unlock()
				return nil
			})
		}(root)
	}

	wg.Wait()
	return res
}
