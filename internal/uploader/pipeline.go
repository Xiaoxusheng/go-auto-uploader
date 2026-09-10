// Package uploader — Pipeline 负责单文件：可选转换 → 路径清洗 → 秒传 → PUT → 落账。
package uploader

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"upload/internal/convert"
	"upload/internal/hashstore"
	"upload/internal/naming"
	"upload/internal/remote"
	"upload/internal/storage"
)

// Pipeline 上传编排依赖（由 app 注入）。
type Pipeline struct {
	SafeBaseDir   string
	ConvertMP4    func() bool
	FFmpegPath    func() string
	HashDB        *hashstore.Store
	History       *storage.HistoryStore
	Success       *storage.SuccessStore
	DirStatus     *storage.DirStatusStore
	MarkDirty     func()
	OnUpload      func(ctx context.Context, local, remotePath string, size int64) bool
	RecordSuccess func(remotePath, name string, size int64)
	Broadcast     func(typ string, payload any)
}

// IsTS / IsArtifact re-export convert helpers.
var (
	IsTS       = convert.IsTS
	IsArtifact = convert.IsArtifact
)

// PreparePath 将本地绝对路径映射为远端路径。
func (p *Pipeline) PreparePath(local, root string) (name, remotePath string, ok bool) {
	rel, err := filepath.Rel(root, local)
	if err != nil {
		return "", "", false
	}
	name = naming.CleanFileName(filepath.Base(rel))
	remotePath = naming.BuildRemotePath(p.SafeBaseDir, filepath.Dir(rel), filepath.Base(rel))
	return name, remotePath, true
}

// HandleFile 完整处理一个本地文件。
func (p *Pipeline) HandleFile(ctx context.Context, path string, roots []string) {
	if convert.IsArtifact(path) {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[FILE][ERR] 无法获取文件状态 %s: %v", path, err)
		return
	}
	if info.Size() == 0 {
		log.Printf("[FILE][SKIP] 拦截到 0 字节死文件: %s", path)
		_ = os.Remove(path)
		return
	}

	var originalTS string
	if p.ConvertMP4 != nil && p.ConvertMP4() && convert.IsTS(path) {
		ff := ""
		if p.FFmpegPath != nil {
			ff = p.FFmpegPath()
		}
		log.Printf("[CONVERT] 🎬 开始 TS→MP4 封装: %s (%.2f MB)", filepath.Base(path), float64(info.Size())/1024/1024)
		if mp4Path, cerr := convert.TSToMP4(path, ff); cerr == nil {
			originalTS = path
			path = mp4Path
			if ni, nerr := os.Stat(path); nerr == nil {
				info = ni
			} else {
				log.Printf("[CONVERT] 转换后无法读取 MP4，回退原 TS: %v", nerr)
				path = originalTS
				originalTS = ""
				_ = os.Remove(mp4Path)
			}
		} else {
			log.Printf("[CONVERT] ⚠️ 转换失败，将直接上传原 TS: %v", cerr)
		}
	}

	root := naming.DetectRoot(path, roots)
	if root == "" {
		log.Println("[SKIP][NO_ROOT_MATCH] 找不到匹配的根目录:", path)
		return
	}
	name, remotePath, ok := p.PreparePath(path, root)
	if !ok {
		return
	}

	hash := hashstore.FileHash(path)
	if hash != "" && p.HashDB != nil && p.HashDB.Exists(hash) {
		log.Println("[SKIP][HASH] 秒传触发:", path)
		if p.History != nil {
			p.History.Add(storage.HistoryRecord{
				UploadTime: time.Now().Format("2006-01-02 15:04:05"),
				Name:       name,
				Size:       info.Size(),
				LocalPath:  path,
				Remote:     remotePath,
				Status:     "success(秒传)",
			})
		}
		p.bumpDir(root, info.Size())
		if p.Broadcast != nil {
			p.Broadcast("taskDone", map[string]any{"status": "success", "size": info.Size()})
		}
		_ = os.Remove(path)
		if originalTS != "" {
			_ = os.Remove(originalTS)
		}
		return
	}

	if p.OnUpload == nil || !p.OnUpload(ctx, path, remotePath, info.Size()) {
		return
	}

	if p.HashDB != nil {
		p.HashDB.Save(hash)
	}
	_ = os.Remove(path)
	if originalTS != "" {
		_ = os.Remove(originalTS)
	}
	if p.RecordSuccess != nil {
		p.RecordSuccess(remotePath, name, info.Size())
	}
	p.bumpDir(root, info.Size())
}

func (p *Pipeline) bumpDir(root string, size int64) {
	if p.DirStatus == nil {
		return
	}
	if ds, exists := p.DirStatus.Get(root); exists {
		ds.Mu.Lock()
		if ds.PendingFiles > 0 {
			ds.PendingFiles--
		}
		ds.UploadedFiles++
		ds.UploadedSize += size
		ds.TotalFiles = ds.PendingFiles + ds.UploadedFiles
		ds.Mu.Unlock()
	}
	if p.MarkDirty != nil {
		p.MarkDirty()
	}
}

// DetectRoot re-export
func DetectRoot(path string, roots []string) string { return naming.DetectRoot(path, roots) }

// unused remote import keep for type in signature docs
var _ = remote.OpenListClient{}
