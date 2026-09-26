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
	poseFeatDataMu sync.Mutex
	poseFeatData   struct {
		modTime time.Time
		size    int64
		data    map[string]json.RawMessage // clip → {fps, feats}
	}
	poseClipsMu    sync.Mutex
	poseClipsCache struct {
		modTime time.Time
		clips   []poseClip
	}
	poseSpansMu   sync.Mutex
	poseSpansData struct {
		modTime time.Time
		spans   map[string][]map[string]any // clip → 原始预标段
	}
)

type poseClip struct {
	Clip    string `json:"clip"`
	Prior   string `json:"prior"`
	Seconds int    `json:"seconds"`
	Dance   int    `json:"dance"`
	Closeup int    `json:"closeup"`
	None    int    `json:"none"`
	Other   int    `json:"other"`
	Model   bool   `json:"model"`
	Thumb   bool   `json:"thumb"`
}

// poseReadClips 读片池配置（mtime 缓存，ingest 每片原子落盘，读侧容忍偶尔失败）。
// 训练数据不存在（如服务器上没有训练管线）→ 返回空池而非报错，页面走空态。
func poseReadClips() ([]poseClip, error) {
	path := filepath.Join(poseTrainRoot, "_pose_pilot", "clips_config.json")
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []poseClip{}, nil
		}
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
	spans := make(map[string][]map[string]any, len(raw))
	framesRoot := filepath.Join(poseTrainRoot, "_pose_pilot", "frames")
	for _, r := range raw {
		c := poseClip{Clip: r.Clip, Prior: r.Prior, Model: r.Model}
		sp := make([]map[string]any, 0, len(r.Spans))
		for _, s := range r.Spans {
			sp = append(sp, map[string]any{"label": s.Label, "start": toInt(s.Start), "end": toInt(s.End)})
		}
		spans[r.Clip] = sp
		for _, x := range sp {
			s, e := toInt(x["start"]), toInt(x["end"])
			if e > c.Seconds {
				c.Seconds = e
			}
			switch x["label"] {
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
	poseSpansMu.Lock()
	poseSpansData.modTime = st.ModTime()
	poseSpansData.spans = spans
	poseSpansMu.Unlock()
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
	// 本机没有训练产物（如服务器只负责录制/上传）→ train_ready=false，页面显示空态提示
	clipsPath := filepath.Join(poseTrainRoot, "_pose_pilot", "clips_config.json")
	trainReady := false
	if _, err := os.Stat(clipsPath); err == nil {
		trainReady = true
	}
	// 磁盘余量：训练根目录存在取该卷；否则回落进程所在卷（服务器场景）
	diskPath := poseTrainRoot
	if !trainReady {
		diskPath = "."
	}
	summary := map[string]any{
		"pool":         len(clips),
		"features":     poseCountFeatures(),
		"gold_clips":   poseCountGold(),
		"train_ready":  trainReady,
		"gate":         app.AppCfg().Builtin.HighlightPoseGate,
		"disk_free_gb": float64(getDiskFreeSpaceStd(diskPath)) / 1073741824,
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

// handlePoseTrainingThumb 片池缩略图 / 帧查看器取帧：frames/<clip>/ 的 JPEG 直出
// （<img> 经登录会话 Cookie 鉴权，同封面图反代模式）。idx=0 基帧序号，缺省取中间帧。
func (s *Server) handlePoseTrainingThumb(w http.ResponseWriter, r *http.Request) {
	clip := filepath.Base(r.URL.Query().Get("clip")) // 防路径穿越
	if clip == "" || clip == "." || clip == "/" {
		s.sendJSONError(w, r, http.StatusBadRequest, "clip 必填")
		return
	}
	var target string
	if v := r.URL.Query().Get("idx"); v != "" {
		idx, err := strconv.Atoi(v)
		if err != nil {
			s.sendJSONError(w, r, http.StatusBadRequest, "idx 非法")
			return
		}
		f, ferr := poseFrameFile(clip, idx)
		if ferr != nil {
			s.sendJSONError(w, r, http.StatusNotFound, "无帧")
			return
		}
		target = f
	} else {
		files, _ := filepath.Glob(filepath.Join(poseTrainRoot, "_pose_pilot", "frames", clip, "f_*.jpg"))
		if len(files) == 0 {
			s.sendJSONError(w, r, http.StatusNotFound, "无帧")
			return
		}
		sort.Strings(files)
		target = files[len(files)/2]
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, target)
}

// poseFeatDataMap 特征全量缓存（20MB 级文件，mtime+size 变化才重新解析）。
func poseFeatDataMap() (map[string]json.RawMessage, error) {
	path := filepath.Join(poseTrainRoot, "pose_features_go.json")
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	poseFeatDataMu.Lock()
	defer poseFeatDataMu.Unlock()
	if poseFeatData.data != nil && st.ModTime().Equal(poseFeatData.modTime) && st.Size() == poseFeatData.size {
		return poseFeatData.data, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil, err
	}
	poseFeatData.modTime, poseFeatData.size, poseFeatData.data = st.ModTime(), st.Size(), m
	return m, nil
}

// poseLogTail 读最新训练日志尾部行（含文件名前缀），供实时过程卡展示。
func poseLogTail(n int) []string {
	matches, _ := filepath.Glob(filepath.Join(poseTrainRoot, "*ingest*.log"))
	nightly, _ := filepath.Glob(filepath.Join(poseTrainRoot, "nightly_train*.log"))
	hourly, _ := filepath.Glob(filepath.Join(poseTrainRoot, "autotrain_hourly.log"))
	matches = append(matches, nightly...)
	matches = append(matches, hourly...)
	sort.Slice(matches, func(i, j int) bool {
		fi, _ := os.Stat(matches[i])
		fj, _ := os.Stat(matches[j])
		return fi.ModTime().After(fj.ModTime())
	})
	if len(matches) > 1 {
		matches = matches[:1]
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
			out = append(out, base+" │ "+strings.TrimSpace(l))
		}
	}
	return out
}

// handlePoseTrainingLive 实时过程：管线阶段 / 最近处理片 / 队列余量 / 日志尾。
func (s *Server) handlePoseTrainingLive(w http.ResponseWriter, r *http.Request) {
	logs := poseLogTail(14)
	parseLines := poseLogTail(800) // last_done 解析用更深的历史
	// 运行判定：最新训练日志 90 秒内有写入 → 管线活跃
	running := false
	stage := "idle"
	matches, _ := filepath.Glob(filepath.Join(poseTrainRoot, "*ingest*.log"))
	hourly, _ := filepath.Glob(filepath.Join(poseTrainRoot, "autotrain_hourly.log"))
	matches = append(matches, hourly...)
	newest := time.Time{}
	for _, p := range matches {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	if !newest.IsZero() && time.Since(newest) < 120*time.Second {
		running = true
		stage = "ingest"
	}
	// 最近一片：解析日志尾部的 [i/n] 行（200 行深度，兼容守护多轮日志）
	lastDone := map[string]any{}
	for i := len(parseLines) - 1; i >= 0; i-- {
		l := parseLines[i]
		if a := strings.Index(l, "["); a >= 0 {
			if b := strings.Index(l[a:], "]"); b > 0 {
				idxStr := l[a+1 : a+b]
				if k := strings.Index(idxStr, "/"); k > 0 {
					idx, e1 := strconv.Atoi(idxStr[:k])
					total, e2 := strconv.Atoi(idxStr[k+1:])
					rest := l[a+b+2:]
					if e1 == nil && e2 == nil && (strings.Contains(rest, "帧 / ") || strings.Contains(rest, "秒特征")) {
						parts := strings.SplitN(rest, ":", 2)
						lastDone = map[string]any{"idx": idx, "total": total, "clip": strings.TrimSpace(parts[0])}
						if len(parts) == 2 {
							lastDone["detail"] = strings.TrimSpace(parts[1])
						}
						break
					}
				}
			}
		}
	}
	clips, _ := poseReadClips()
	configured := map[string]bool{}
	for _, c := range clips {
		configured[c.Clip] = true
	}
	// 队列余量：downloads 里的原片还没进池的数量。
	// 目录树大（数千文件），同步遍历会拖慢页面轮询 → 后台刷新 + 立即返回上次值
	remaining := poseQueueRemainingAsync(configured)
	s.sendJSONSuccess(w, r, map[string]any{
		"running": running, "stage": stage,
		"last_done": lastDone, "logs": logs,
		"pool": len(clips), "features": poseCountFeatures(),
		"queue_remaining": remaining,
		"disk_free_gb":    float64(getDiskFreeSpaceStd(".")) / 1073741824,
	})
}

// handlePoseTrainingClip 单片详情：预标段 + 每秒姿态特征 + 8s 窗判定（标注数据可视化）。
func (s *Server) handlePoseTrainingClip(w http.ResponseWriter, r *http.Request) {
	clip := filepath.Base(r.URL.Query().Get("clip"))
	if clip == "" || clip == "." || clip == "/" {
		s.sendJSONError(w, r, http.StatusBadRequest, "clip 必填")
		return
	}
	resp := map[string]any{"clip": clip}
	var prior string
	var spans []map[string]any
	if err := func() error {
		if _, err := poseReadClips(); err != nil {
			return err
		}
		poseSpansMu.Lock()
		defer poseSpansMu.Unlock()
		spans = poseSpansData.spans[clip]
		return nil
	}(); err == nil {
		resp["spans"] = spans
		_ = prior
	}
	// 每秒姿态特征 + 8s 窗判定
	if feats, err := poseFeatDataMap(); err == nil {
		if raw, ok := feats[clip]; ok {
			var e struct {
				FPS   int          `json:"fps"`
				Feats [][5]float64 `json:"feats"`
			}
			if json.Unmarshal(raw, &e) == nil {
				const detMin, visMin, faceMax = 0.2, 0.6, 0.14
				wins := make([]map[string]any, 0, len(e.Feats)/8+1)
				for st := 0; st < len(e.Feats); st += 8 {
					en := st + 8
					if en > len(e.Feats) {
						en = len(e.Feats)
					}
					det, sv, sf, se := 0, 0.0, 0.0, 0.0
					for _, f := range e.Feats[st:en] {
						if f[4] == 1 {
							det++
							sv += f[0]
							sf += f[1]
							se += f[2]
						}
					}
					label := "none"
					if det > 0 {
						mv, mf := sv/float64(det), sf/float64(det)
						switch {
						case float64(det)/float64(en-st) < detMin:
							label = "none"
						case mv >= visMin && mf <= faceMax:
							label = "dance"
						case mf > faceMax:
							label = "closeup"
						default:
							label = "other"
						}
						se = se / float64(det)
					}
					wins = append(wins, map[string]any{
						"s": st, "det": float64(det) / float64(en-st),
						"vis": sv / float64(max(det, 1)), "face": sf / float64(max(det, 1)),
						"ext": se, "label": label,
					})
				}
				resp["fps"] = e.FPS
				resp["feats"] = e.Feats
				resp["windows"] = wins
			}
		}
	}
	// 帧文件数
	files, _ := filepath.Glob(filepath.Join(poseTrainRoot, "_pose_pilot", "frames", clip, "f_*.jpg"))
	resp["frames"] = len(files)
	s.sendJSONSuccess(w, r, resp)
}

// poseFrameFile 按 0 基序号取帧文件（越界取最后一帧）。
func poseFrameFile(clip string, idx int) (string, error) {
	dir := filepath.Join(poseTrainRoot, "_pose_pilot", "frames", clip)
	files, _ := filepath.Glob(filepath.Join(dir, "f_*.jpg"))
	if len(files) == 0 {
		return "", os.ErrNotExist
	}
	sort.Strings(files)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(files) {
		idx = len(files) - 1
	}
	return files[idx], nil
}

var (
	poseQueueMu    sync.Mutex
	poseQueueCache struct {
		at         time.Time
		remaining  int
		refreshing bool
	}
)

// poseQueueRemainingAsync 队列余量后台刷新：缓存 5 分钟；过期时起协程重算并
// 先返回上次值，保证 /live 轮询永不因磁盘遍历阻塞。
func poseQueueRemainingAsync(configured map[string]bool) int {
	poseQueueMu.Lock()
	defer poseQueueMu.Unlock()
	if time.Since(poseQueueCache.at) < 5*time.Minute {
		return poseQueueCache.remaining
	}
	if !poseQueueCache.refreshing {
		poseQueueCache.refreshing = true
		dl := os.Getenv("DOWNLOADS_ROOT")
		if dl == "" {
			dl = "D:/upload/downloads"
		}
		go func() {
			n := 0
			_ = filepath.Walk(dl, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				name := strings.ToLower(info.Name())
				if !strings.HasSuffix(name, ".ts") || strings.Contains(info.Name(), "高光") {
					return nil
				}
				if !configured[strings.TrimSuffix(info.Name(), filepath.Ext(info.Name()))] {
					n++
				}
				return nil
			})
			poseQueueMu.Lock()
			poseQueueCache.remaining = n
			poseQueueCache.at = time.Now()
			poseQueueCache.refreshing = false
			poseQueueMu.Unlock()
		}()
	}
	return poseQueueCache.remaining
}
