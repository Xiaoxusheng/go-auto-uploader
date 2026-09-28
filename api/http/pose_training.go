package httpapi

// 「姿态训练」控制台页签后端：只读展示本机训练管线落盘数据
// （_diag/train：片池/姿态特征/金标/重定标结果/入池日志 + 帧缩略图）。
// 根目录可用环境变量 POSE_TRAIN_ROOT 覆盖（默认 D:/upload/_diag/train）。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"upload/internal/app"
	"upload/internal/config"
	"upload/internal/pose"
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

// poseClipRe 片名「主播_日期_时间_序号」中的固定日期段（主播名可含下划线/emoji，按日期格式切分）。
var poseClipRe = regexp.MustCompile(`^(.+)_(\d{4}-\d{2}-\d{2})_(\d{2}-\d{2}-\d{2})_\d+$`)

// poseClipStreamer 从片名解析主播名（不匹配返回空串，调用方回退片名展示）。
func poseClipStreamer(clip string) string {
	if m := poseClipRe.FindStringSubmatch(clip); m != nil {
		return m[1]
	}
	return ""
}

// poseCurrentClip frames 下 mtime 最新的片目录：入池时抽帧先落盘、随后逐帧推理，
// 推理期间目录不再变化，故管线活跃时它就是正在处理的片。
// 返回片名、目录 mtime、已抽帧数（供前端区分抽帧/推理阶段）。
func poseCurrentClip() (string, time.Time, int) {
	entries, err := os.ReadDir(filepath.Join(poseTrainRoot, "_pose_pilot", "frames"))
	if err != nil {
		return "", time.Time{}, 0
	}
	var newestName string
	var newest time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(newest) {
			newest, newestName = fi.ModTime(), e.Name()
		}
	}
	if newestName == "" {
		return "", time.Time{}, 0
	}
	frames, _ := filepath.Glob(filepath.Join(poseTrainRoot, "_pose_pilot", "frames", newestName, "f_*.jpg"))
	return newestName, newest, len(frames)
}

// poseQueueHead 扫描 downloads 待入池原片：返回剩余总数 + 最旧的 n 片（先来先处理）。
func poseQueueHead(dl string, configured map[string]bool, n int) (int, []map[string]any) {
	type pendItem struct {
		clip string
		mod  time.Time
	}
	remaining, pending := 0, []pendItem{}
	_ = filepath.Walk(dl, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".ts") || strings.Contains(info.Name(), "高光") {
			return nil
		}
		clip := strings.TrimSuffix(info.Name(), filepath.Ext(info.Name()))
		if configured[clip] {
			return nil
		}
		remaining++
		pending = append(pending, pendItem{clip: clip, mod: info.ModTime()})
		return nil
	})
	sort.Slice(pending, func(i, j int) bool { return pending[i].mod.Before(pending[j].mod) })
	head := make([]map[string]any, 0, n)
	for i, it := range pending {
		if i >= n {
			break
		}
		head = append(head, map[string]any{"clip": it.clip, "streamer": poseClipStreamer(it.clip)})
	}
	return remaining, head
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

// poseRecentDone 解析日志尾部最近 n 个已完成片（[i/n] 完成行，倒序去重），
// 供驾驶舱「已入池」缩略图列展示。
func poseRecentDone(n int) []map[string]any {
	lines := poseLogTail(800)
	seen := map[string]bool{}
	out := []map[string]any{}
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		l := lines[i]
		a := strings.Index(l, "[")
		if a < 0 {
			continue
		}
		b := strings.Index(l[a:], "]")
		if b <= 0 {
			continue
		}
		rest := l[a+b+2:]
		if !strings.Contains(rest, "帧 / ") && !strings.Contains(rest, "秒特征") {
			continue
		}
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) < 2 {
			continue
		}
		clip := strings.TrimSpace(parts[0])
		if clip == "" || seen[clip] {
			continue
		}
		seen[clip] = true
		out = append(out, map[string]any{
			"clip": clip, "streamer": poseClipStreamer(clip),
			"detail": strings.TrimSpace(parts[1]),
		})
	}
	return out
}

// poseRecentSkipped 解析日志尾部最近 n 个「帧不足，跳过」片（含帧数），
// 供驾驶舱跳过走向动画与跳过列表展示。
func poseRecentSkipped(n int) []map[string]any {
	lines := poseLogTail(800)
	seen := map[string]bool{}
	out := []map[string]any{}
	mark := "帧不足，跳过 "
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		l := lines[i]
		a := strings.Index(l, mark)
		if a < 0 {
			continue
		}
		rest := strings.TrimSpace(l[a+len(mark):])
		open := strings.LastIndex(rest, "(")
		if open < 0 || !strings.HasSuffix(rest, ")") {
			continue
		}
		frames, err := strconv.Atoi(strings.TrimSuffix(rest[open+1:], ")"))
		if err != nil || frames < 0 {
			continue
		}
		clip := strings.TrimSpace(rest[:open])
		if clip == "" || seen[clip] {
			continue
		}
		seen[clip] = true
		out = append(out, map[string]any{"clip": clip, "streamer": poseClipStreamer(clip), "frames": frames})
	}
	return out
}

// —— 重定标最优参数自动应用（带防抖）——
// 比对基准来自 autogold_result.json 的 live 行（生产现值 F1，由 sweep 脚本计算）：
// 最优 F1 连续 3 轮高于现值 ≥0.01 才自动写入，单轮波动不触发。
//
// ⚠️ 默认关闭（2026-09-27）。原因：sweep 的 best 与 live 都在 gold_review **同一份
// 数据**上计算，没有留出验证集——自动应用等于「在评估集上做模型选择并直接部署」。
// 实测证据：autogold_result.json 里 best 与 live 逐字段相等（0.70/0.12/0.30，F1 0.784），
// 正是被本机制反复写入的结果；而冻结集 v2 段级 F1 只有 0.211（开封 #6，未过线）。
// 重新启用前必须满足：sweep 输出带留出集（按片分组）指标，且本处判据改用该指标。

var poseApplyMu sync.Mutex

type poseApplyState struct {
	AutoApply   bool   `json:"auto_apply"`   // 开关，默认关（见上方说明）
	Streak      int    `json:"streak"`       // 连续更优轮数
	LastSeen    string `json:"last_seen"`    // 上次处理的 sweep generated_at
	LastApplied string `json:"last_applied"` // 最近一次应用的参数描述
}

func poseApplyStatePath() string { return filepath.Join(poseTrainRoot, "autogold_apply_state.json") }

func poseLoadApplyState() poseApplyState {
	st := poseApplyState{AutoApply: false}
	if b, err := os.ReadFile(poseApplyStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func poseSaveApplyState(st poseApplyState) {
	if b, err := json.MarshalIndent(st, "", " "); err == nil {
		_ = os.WriteFile(poseApplyStatePath(), b, 0o644)
	}
}

// poseApplyGateThresholds 纯函数：把 sweep 最优三阈值写进门配置副本。
// 第二返回值 false = 门未启用，无参数可写。
func poseApplyGateThresholds(cfg *config.Config, vis, face, det float64) (config.HighlightPoseGateConfig, bool) {
	if cfg.Builtin.HighlightPoseGate == nil || !cfg.Builtin.HighlightPoseGate.Enable {
		return config.HighlightPoseGateConfig{}, false
	}
	g := *cfg.Builtin.HighlightPoseGate
	g.VisMin, g.FaceMax, g.DetMin = vis, face, det
	cfg.Builtin.HighlightPoseGate = &g
	return g, true
}

// poseMaybeAutoApply 每次 /live 轮询轻触：sweep 结果更新时评估防抖条件，
// 达标（连续 3 轮更优）自动写入生产配置并在训练日志留痕。返回最新状态与本轮是否刚应用。
func poseMaybeAutoApply() (poseApplyState, string) {
	poseApplyMu.Lock()
	defer poseApplyMu.Unlock()
	st := poseLoadApplyState()
	b, err := os.ReadFile(filepath.Join(poseTrainRoot, "autogold_result.json"))
	if err != nil {
		return st, ""
	}
	var res struct {
		GeneratedAt string `json:"generated_at"`
		Best        *struct {
			Vis  float64 `json:"vis"`
			Face float64 `json:"face"`
			Det  float64 `json:"det"`
			F1   float64 `json:"F1"`
		} `json:"best"`
		Live *struct {
			F1 float64 `json:"F1"`
		} `json:"live"`
	}
	if json.Unmarshal(b, &res) != nil || res.GeneratedAt == "" || res.Best == nil {
		return st, ""
	}
	if res.GeneratedAt == st.LastSeen {
		return st, "" // 已处理过这轮 sweep
	}
	st.LastSeen = res.GeneratedAt
	applied := ""
	defer func() { poseSaveApplyState(st) }()
	if !st.AutoApply || res.Live == nil {
		return st, ""
	}
	if res.Best.F1 >= res.Live.F1+0.01 {
		st.Streak++
	} else {
		st.Streak = 0
	}
	if st.Streak < 3 {
		return st, ""
	}
	cfg := app.AppCfg()
	g, ok := poseApplyGateThresholds(&cfg, res.Best.Vis, res.Best.Face, res.Best.Det)
	if !ok {
		return st, ""
	}
	app.CfgStore.Replace(cfg)
	app.SaveConfigToFile()
	st.Streak = 0
	st.LastApplied = fmt.Sprintf("%s vis %.2f / face %.2f / det %.2f (F1 %.3f)",
		time.Now().Format("01-02 15:04"), g.VisMin, g.FaceMax, g.DetMin, res.Best.F1)
	if f, err := os.OpenFile(filepath.Join(poseTrainRoot, "autotrain_hourly.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "=== %s 自动应用重定标最优: vis %.2f / face %.2f / det %.2f (F1 %.3f，连续 3 轮更优)\n",
			time.Now().Format("2006-01-02 15:04:05"), g.VisMin, g.FaceMax, g.DetMin, res.Best.F1)
		_ = f.Close()
	}
	applied = "自动应用重定标最优: vis " + strconv.FormatFloat(g.VisMin, 'f', 2, 64) +
		" / face " + strconv.FormatFloat(g.FaceMax, 'f', 2, 64) +
		" / det " + strconv.FormatFloat(g.DetMin, 'f', 2, 64)
	return st, applied
}

// handlePoseTrainingApplyBest 一键应用重定标最优三阈值到生产门配置。
func (s *Server) handlePoseTrainingApplyBest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	b, err := os.ReadFile(filepath.Join(poseTrainRoot, "autogold_result.json"))
	if err != nil {
		s.sendJSONError(w, r, http.StatusNotFound, "暂无重定标结果")
		return
	}
	var res struct {
		GeneratedAt string `json:"generated_at"`
		Best        *struct {
			Vis  float64 `json:"vis"`
			Face float64 `json:"face"`
			Det  float64 `json:"det"`
			F1   float64 `json:"F1"`
		} `json:"best"`
	}
	if json.Unmarshal(b, &res) != nil || res.Best == nil {
		s.sendJSONError(w, r, http.StatusNotFound, "重定标结果不可读")
		return
	}
	cfg := app.AppCfg()
	g, ok := poseApplyGateThresholds(&cfg, res.Best.Vis, res.Best.Face, res.Best.Det)
	if !ok {
		s.sendJSONError(w, r, http.StatusConflict, "姿态门未启用，无参数可写")
		return
	}
	app.CfgStore.Replace(cfg)
	app.SaveConfigToFile()
	poseApplyMu.Lock()
	st := poseLoadApplyState()
	st.Streak = 0
	st.LastSeen = res.GeneratedAt
	st.LastApplied = fmt.Sprintf("%s 手动应用 vis %.2f / face %.2f / det %.2f (F1 %.3f)",
		time.Now().Format("01-02 15:04"), g.VisMin, g.FaceMax, g.DetMin, res.Best.F1)
	poseSaveApplyState(st)
	poseApplyMu.Unlock()
	log.Printf("[CONTROL] ⚙️ 一键应用重定标最优: vis %.2f / face %.2f / det %.2f (F1 %.3f)",
		g.VisMin, g.FaceMax, g.DetMin, res.Best.F1)
	s.sendJSONSuccess(w, r, map[string]any{"gate": g, "applied": st.LastApplied})
}

// handlePoseTrainingAutoApply 自动应用开关。
func (s *Server) handlePoseTrainingAutoApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req struct {
		Enable *bool `json:"enable"`
	}
	if err := s.parseEncryptedRequest(r, &req); err != nil || req.Enable == nil {
		s.sendJSONError(w, r, http.StatusBadRequest, "缺少 enable 字段")
		return
	}
	poseApplyMu.Lock()
	st := poseLoadApplyState()
	st.AutoApply = *req.Enable
	poseSaveApplyState(st)
	poseApplyMu.Unlock()
	s.sendJSONSuccess(w, r, map[string]any{"apply_state": st})
}

// handlePoseTrainingLive 实时过程：管线阶段 / 最近处理片 / 队列余量 / 日志尾。
func (s *Server) handlePoseTrainingLive(w http.ResponseWriter, r *http.Request) {
	logs := poseLogTail(14)
	parseLines := poseLogTail(800) // last_done 解析用更深的历史
	// 正在处理片：frames 最新目录（供驾驶舱展示；停止后目录年龄不再冒充运行）
	curClip, curMt, curFrames := poseCurrentClip()
	// 运行判定：训练日志 2 分钟内有写入（手动/连续两个入口都会写日志）
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
	// 上次活动时间：待机时告知用户守护上一轮何时跑过（今天只显时分，跨天带日期）
	lastActivity := ""
	if !newest.IsZero() {
		if newest.Format("2006-01-02") == time.Now().Format("2006-01-02") {
			lastActivity = newest.Format("15:04")
		} else {
			lastActivity = newest.Format("01-02 15:04")
		}
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
	if clip, ok := lastDone["clip"].(string); ok {
		lastDone["streamer"] = poseClipStreamer(clip)
	}
	// 正在处理：frames 最新片目录（与最近完成片同名说明该片刚完稿、下一片尚未抽帧）
	current := map[string]any{}
	if curClip != "" {
		current = map[string]any{
			"clip": curClip, "streamer": poseClipStreamer(curClip),
			"frames": curFrames, "age_sec": int(time.Since(curMt).Seconds()),
		}
	}
	configured := map[string]bool{}
	for _, c := range clips {
		configured[c.Clip] = true
	}
	// 待入池队列：剩余数量 + 队头几片（先来先处理），异步缓存见 poseQueueInfoAsync
	remaining, queueHead := poseQueueInfoAsync(configured)
	applyState, appliedNow := poseMaybeAutoApply()
	if appliedNow != "" {
		logs = append([]string{"autotrain_hourly.log │ " + appliedNow}, logs...)
	}
	s.sendJSONSuccess(w, r, map[string]any{
		"running": running, "stage": stage,
		"last_done": lastDone, "current": current, "logs": logs,
		"recent_done": poseRecentDone(5), "recent_skipped": poseRecentSkipped(5),
		"last_activity": lastActivity, "apply_state": applyState,
		"pool": len(clips), "features": poseCountFeatures(),
		"queue_remaining": remaining, "queue_head": queueHead,
		"disk_free_gb": float64(getDiskFreeSpaceStd(".")) / 1073741824,
	})
}

// handlePoseTrainingClip 单片详情：预标段 + 每秒姿态特征 + 8s 窗判定（标注数据可视化）。
func (s *Server) handlePoseTrainingClip(w http.ResponseWriter, r *http.Request) {
	clip := filepath.Base(r.URL.Query().Get("clip"))
	if clip == "" || clip == "." || clip == "/" {
		s.sendJSONError(w, r, http.StatusBadRequest, "clip 必填")
		return
	}
	resp := map[string]any{"clip": clip, "streamer": poseClipStreamer(clip)}
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
	// 每秒姿态特征 + 8s 窗判定（走 internal/pose 的同一判定函数，
	// 保证页面标签与 review-ingest 落盘的 spans 口径一致）
	if feats, err := poseFeatDataMap(); err == nil {
		if raw, ok := feats[clip]; ok {
			var e struct {
				FPS   int          `json:"fps"`
				Feats [][5]float64 `json:"feats"`
			}
			if json.Unmarshal(raw, &e) == nil {
				wins := make([]map[string]any, 0, len(e.Feats)/8+1)
				for st := 0; st < len(e.Feats); st += 8 {
					en := st + 8
					if en > len(e.Feats) {
						en = len(e.Feats)
					}
					win := make([]pose.FrameFeatures, en-st)
					for i, f := range e.Feats[st:en] {
						win[i] = pose.FrameFeatures{
							Detected: f[4] == 1,
							VisRatio: f[0],
							FaceFrac: f[1],
							ExtH:     f[2],
							Aspect:   f[3],
						}
					}
					ws := pose.AggregateWindow(win, pose.PrelabelDetMin, pose.PrelabelVisMin, pose.PrelabelFaceMax)
					wins = append(wins, map[string]any{
						"s": st, "det": ws.DetRate,
						"vis": ws.VisMean, "face": ws.FaceMean,
						"ext": ws.ExtMean, "label": ws.Label,
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
		head       []map[string]any
		refreshing bool
	}
)

// poseQueueInfoAsync 队列余量+队头后台刷新：缓存 5 分钟；过期时起协程重算并
// 先返回上次值，保证 /live 轮询永不因磁盘遍历阻塞。
func poseQueueInfoAsync(configured map[string]bool) (int, []map[string]any) {
	poseQueueMu.Lock()
	defer poseQueueMu.Unlock()
	if time.Since(poseQueueCache.at) < 5*time.Minute {
		return poseQueueCache.remaining, poseQueueCache.head
	}
	if !poseQueueCache.refreshing {
		poseQueueCache.refreshing = true
		dl := os.Getenv("DOWNLOADS_ROOT")
		if dl == "" {
			dl = "D:/upload/downloads"
		}
		go func() {
			n, head := poseQueueHead(dl, configured, 6)
			poseQueueMu.Lock()
			poseQueueCache.remaining, poseQueueCache.head = n, head
			poseQueueCache.at = time.Now()
			poseQueueCache.refreshing = false
			poseQueueMu.Unlock()
		}()
	}
	return poseQueueCache.remaining, poseQueueCache.head
}
