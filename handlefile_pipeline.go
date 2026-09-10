package main

// 上传 Pipeline 装配：main 侧回调注入 internal/uploader。

import (
	"context"

	"upload/internal/app"
	"upload/internal/recorder"
	"upload/internal/uploader"
)

var uploadPipeline *uploader.Pipeline

func ensureUploadPipeline() *uploader.Pipeline {
	if uploadPipeline != nil {
		return uploadPipeline
	}
	uploadPipeline = &uploader.Pipeline{
		SafeBaseDir: app.SafeBaseDir,
		ConvertMP4:  func() bool { return appCfg().ConvertMP4 },
		FFmpegPath:  recorder.FFmpegBin,
		HashDB:      hashDB,
		History:     historyStore,
		Success:     successStore,
		DirStatus:   dirStatusStore,
		MarkDirty:   markDirStatusDirty,
		OnUpload: func(ctx context.Context, local, remotePath string, size int64) bool {
			return upload(local, remotePath, size)
		},
		RecordSuccess: recordSuccess,
		Broadcast:     broadcastWS,
	}
	return uploadPipeline
}
