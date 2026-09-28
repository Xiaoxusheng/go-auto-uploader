package httpapi

import (
	"net/http"
	"path/filepath"

	"upload/internal/app"
)

// handleHighlightLive 「高光判定 · 实时队列」：把高光分析管线的真实队列信号
// （待分析积压/队头、当前片阶段、今日统计、最近判定）暴露给控制台内置轻量引擎页。
// 数据口径与 highlightPass 的消费顺序一致，见 internal/app HighlightQueueSnapshot。
func (s *Server) handleHighlightLive(w http.ResponseWriter, r *http.Request) {
	s.sendJSONSuccess(w, r, app.HighlightLiveStatus())
}

// handleHighlightThumb 高光源片缩略图：ffmpeg 抽帧 + dataDir 磁盘缓存。
// <img> 直链请求（走会话 Cookie，见 internal/auth 白名单），clip 只取文件名防路径穿越。
func (s *Server) handleHighlightThumb(w http.ResponseWriter, r *http.Request) {
	clip := filepath.Base(r.URL.Query().Get("clip"))
	if clip == "" || clip == "." || clip == "/" || clip == "\\" {
		s.sendJSONError(w, r, http.StatusBadRequest, "clip 必填")
		return
	}
	pos, err := app.HighlightThumb(clip)
	if err != nil {
		s.sendJSONError(w, r, http.StatusNotFound, "无缩略图")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, pos)
}
