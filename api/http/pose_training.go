package httpapi

// 「姿态训练」控制台页签后端：只读展示本机训练管线落盘数据
// （_diag/train：片池/姿态特征/金标/重定标结果/入池日志 + 帧缩略图）。
// 根目录可用环境变量 POSE_TRAIN_ROOT 覆盖（默认 D:/upload/_diag/train）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"upload/internal/app"
)

var poseTrainRoot = func() string {
	if v := os.Getenv("POSE_TRAIN_ROOT"); v != "" {
		return v
	}
	return "D:/upload/_diag/train"
}()

var (
	poseFeatCountMu    sync.Mutex
	poseFeatCountCache struct {
		modTime time.Time
		size    int64
		n       int
	}
	poseClipsMu    sync.Mutex
	poseClipsCache struct {
		modTime time.Time
		clips   []poseClip
	}
)

type poseClip struct {
	Clip    string           `json:"clip"`
	Prior   string           `json:"prior"`
	Seconds int              `json:"seconds"`
	Dance   int              `json:"dance"`
	Closeup int              `json:"closeup"`
	None    int              `json:"none"`
	Other   int              `json:"other"`
	Model   bool             `json:"model"`
	Thumb   bool             `json:"thumb"`
	Spans   []map[string]any `json:"spans,omitempty"`
}

// poseReadClips 读片池配置（mtime 缓存，ingest 每片原子落盘，读侧容忍偶尔失败）。
func poseReadClips() ([]poseClip, error) {
	path := filepath.Join(poseTrainRoot, "_pose_pilot", "clips_config.json")
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	poseClipsMu.Lock()
	defer poseClipsMu.Unlock()
	if poseClipsCache.clips != nil && st.ModTime().Equal(poseClipsCache.modTime) {
		return poseClipsCache.clips, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Clip  string `json:"clip"`
		Prior string `json:"prior"`
		Model bool   `json:"model"`
		Spans []struct {
			Label string `json:"label"`
			Start any    `json:"start"`
			End   any    `json:"end"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	clips := make([]poseClip, 0, len(raw))
	framesRoot := filepath.Join(poseTrainRoot, "_pose_pilot", "frames")
	for _, r := range raw {
		c := poseClip{Clip: r.Clip, Prior: r.Prior, Model: r.Model, Spans: nil}
		for _, sp := range r.Spans {
			s, e := toInt(sp.Start), toInt(sp.End)
			if e > c.Seconds {
				c.Seconds = e
			}
			switch sp.Label {
			case "dance":
				c.Dance += e - s
			case "closeup":
				c.Closeup += e - s
			case "none":
				c.None += e - s
			default:
				c.Other += e - s
			}
		}
		if _, err := os.Stat(filepath.Join(framesRoot, r.Clip)); err == nil {
			c.Thumb = true
		}
		clips = append(clips, c)
	}
	poseClipsCache.modTime = st.ModTime()
	poseClipsCache.clips = clips
	return clips, nil
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// poseCountFeatures 姿态特征 JSON 键数（文件大，按 mtime 缓存计数结果）。
func poseCountFeatures() int {
	path := filepath.Join(poseTrainRoot, "pose_features_go.json") // 与 review-ingest -pose-out 同路径
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	poseFeatCountMu.Lock()
	defer poseFeatCountMu.Unlock()
	if poseFeatCountCache.n > 0 && st.ModTime().Equal(poseFeatCountCache.modTime) && st.Size() == poseFeatCountCache.size {
		return poseFeatCountCache.n
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return 0
	}
	poseFeatCountCache.modTime, poseFeatCountCache.size, poseFeatCountCache.n = st.ModTime(), st.Size(), len(m)
	return len(m)
}

func poseCountGold() int {
	b, err := os.ReadFile(filepath.Join(poseTrainRoot, "_pose_pilot", "gold_review.json"))
	if err != nil {
		return 0
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return 0
	}
	return len(m)
}

// poseTailLogs 取最新两份训练日志的尾部（入池/夜训活动）。
func poseTailLogs(n int) []string {
	matches, _ := filepath.Glob(filepath.Join(poseTrainRoot, "*ingest*.log"))
	nightly, _ := filepath.Glob(filepath.Join(poseTrainRoot, "nightly_train*.log"))
	matches = append(matches, nightly...)
	sort.Slice(matches, func(i, j int) bool {
		fi, _ := os.Stat(matches[i])
		fj, _ := os.Stat(matches[j])
		return fi.ModTime().After(fj.ModTime())
	})
	if len(matches) > 2 {
		matches = matches[:2]
	}
	out := []string{}
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		base := filepath.Base(p)
		for _, l := range lines {
			out = append(out, base+" │ "+l)
		}
	}
	return out
}

// handlePoseTrainingSummary 训练进度总览（进度卡 + 重定标结果 + 门参数）。
func (s *Server) handlePoseTrainingSummary(w http.ResponseWriter, r *http.Request) {
	clips, _ := poseReadClips()
	summary := map[string]any{
		"pool":         len(clips),
		"features":     poseCountFeatures(),
		"gold_clips":   poseCountGold(),
		"gate":         app.AppCfg().Builtin.HighlightPoseGate,
		"disk_free_gb": float64(getDiskFreeSpaceStd(poseTrainRoot)) / 1073741824,
		"logs":         poseTailLogs(30),
	}
	if b, err := os.ReadFile(filepath.Join(poseTrainRoot, "autogold_result.json")); err == nil {
		var sweep map[string]any
		if json.Unmarshal(b, &sweep) == nil {
			summary["sweep"] = sweep
		}
	}
	s.sendJSONSuccess(w, r, summary)
}

// handlePoseTrainingClips 片池列表（分页 + 子串搜索，新片在前）。
func (s *Server) handlePoseTrainingClips(w http.ResponseWriter, r *http.Request) {
	clips, err := poseReadClips()
	if err != nil {
		s.sendJSONError(w, r, http.StatusInternalServerError, "读取片池失败: "+err.Error())
		return
	}
	q := strings.ToLower(r.URL.Query().Get("q"))
	items := make([]poseClip, 0, len(clips))
	for i := len(clips) - 1; i >= 0; i-- { // 新入池在前
		c := clips[i]
		if q != "" && !strings.Contains(strings.ToLower(c.Clip), q) {
			continue
		}
		c.Spans = nil // 列表页不带 spans，减小载荷
		items = append(items, c)
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	start := offset
	if start > len(items) {
		start = len(items)
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	s.sendJSONSuccess(w, r, map[string]any{
		"total": len(items), "offset": start, "items": items[start:end],
	})
}

// handlePoseTrainingThumb 片池缩略图：frames/<clip>/ 取中间帧（JPEG 直出，
// <img> 经登录会话 Cookie 鉴权，同封面图反代模式）。
func (s *Server) handlePoseTrainingThumb(w http.ResponseWriter, r *http.Request) {
	clip := filepath.Base(r.URL.Query().Get("clip")) // 防路径穿越
	if clip == "" || clip == "." || clip == "/" {
		s.sendJSONError(w, r, http.StatusBadRequest, "clip 必填")
		return
	}
	dir := filepath.Join(poseTrainRoot, "_pose_pilot", "frames", clip)
	files, _ := filepath.Glob(filepath.Join(dir, "f_*.jpg"))
	if len(files) == 0 {
		s.sendJSONError(w, r, http.StatusNotFound, "无帧")
		return
	}
	sort.Strings(files)
	mid := files[len(files)/2]
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, mid)
}
