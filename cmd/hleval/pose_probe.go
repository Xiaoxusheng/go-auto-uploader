// pose-probe 节奏/位移特征探针（开封#5 正解的探针阶段，见
// _diag/train/UNSEAL5_20260926.md「生产建议」与 docs/highlight-spatial-de.md §26）。
//
// 问题：gesture（手势聊天/轻晃）窗被门系统性判舞（71.4%），阈值层全网格压不下，
// 需验证「位移模式/构图」候选特征能否分离 dance vs gesture。
//
// 口径：
//   - 金标窗（gold_review.json，gesture 已入集）× 1fps 帧目录逐窗重推理关键点
//     （pose_features_go.json 只有 det/vis/face 标量，不含坐标），缓存增量断点续跑
//   - 位移按躯干长（肩中点↔髋中点）归一化，帧间除以间隔秒数（1fps 缺帧时口径统一）
//   - 评估：逐特征 AUC（dance vs gesture 为主，dance vs 其他类防破坏已验证域），
//     再模拟「现有门 ∧ 特征否决」——gesture 判舞率下降量 vs 舞误杀率是决策数字
//   - 冻结集 v2 的 GT 封存不读、其窗口不入探针，防开封#6 变 in-sample
//
// §24/§25 已证伪 1fps/5fps 节拍耦合（fBand/ac/domFreq），本命令不测节拍族，
// 只测位移模式（torso_ratio / wrist_ankle_ratio 等）与构图（ankle_vis）。
//
// 用法：
//
//	hleval pose-probe [-gold <json>] [-frames <dir>] [-pose <json>] [-cache <json>]
//	                 [-out <json>] [-dll ...] [-model ...] [-limit N]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"upload/internal/pose"
)

const (
	ppWinSec      = 8    // 窗长（秒），与门口径一致
	ppMinFrames   = 4    // 窗内有效帧下限（少于则特征全 NaN）
	ppConf        = 0.5  // 腕/踝可见置信阈值
	ppTorsoConf   = 0.3  // 躯干四点置信阈值（归一化基准必须可靠）
	ppMaxGapSec   = 3    // 位移对最大间隔秒（更大不可靠，跳过）
	ppTorsoEps    = 0.05 // torso_ratio 分母防零项（躯干长/秒）
	ppAnkleVisMin = 0.25 // wrist_ankle_ratio 生效所需最低双踝可见帧占比
	ppMinSeg      = 0.03 // dir_reversal 位移段最小长度（躯干长占比，滤静止噪声）
)

// ppFrame 单帧关键点（原图像素坐标），只存躯干中点与腕踝——位移特征够用。
type ppFrame struct {
	Sec float64 `json:"sec"`
	// 肩中点 / 髋中点（躯干归一化基准）
	MSX float64 `json:"msx"`
	MSY float64 `json:"msy"`
	MHX float64 `json:"mhx"`
	MHY float64 `json:"mhy"`
	// 左腕 / 右腕（坐标+置信度）
	LWX float64 `json:"lwx"`
	LWY float64 `json:"lwy"`
	LWC float64 `json:"lwc"`
	RWX float64 `json:"rwx"`
	RWY float64 `json:"rwy"`
	RWC float64 `json:"rwc"`
	// 左踝 / 右踝（坐标+置信度）
	LAX float64 `json:"lax"`
	LAY float64 `json:"lay"`
	LAC float64 `json:"lac"`
	RAX float64 `json:"rax"`
	RAY float64 `json:"ray"`
	RAC float64 `json:"rac"`
	// 构形特征用的其余点（pose2-probe，§29）：鼻/眼/耳/肩/肘/膝
	NX   float64 `json:"nx"`
	NY   float64 `json:"ny"`
	NC   float64 `json:"nc"`
	LEX  float64 `json:"lex"`
	LEY  float64 `json:"ley"`
	LEC  float64 `json:"lec"`
	REX  float64 `json:"rex"`
	REY  float64 `json:"rey"`
	REC  float64 `json:"rec"`
	LARX float64 `json:"larx"`
	LARY float64 `json:"lary"`
	LARC float64 `json:"larc"`
	RARX float64 `json:"rarx"`
	RARY float64 `json:"rary"`
	RARC float64 `json:"rarc"`
	LSX  float64 `json:"lsx"`
	LSY  float64 `json:"lsy"`
	LSC  float64 `json:"lsc"`
	RSX  float64 `json:"rsx"`
	RSY  float64 `json:"rsy"`
	RSC  float64 `json:"rsc"`
	LELX float64 `json:"lelx"`
	LELY float64 `json:"lely"`
	LELC float64 `json:"lelc"`
	RELX float64 `json:"relx"`
	RELY float64 `json:"rely"`
	RELC float64 `json:"relc"`
	LKNX float64 `json:"lknx"`
	LKNY float64 `json:"lkny"`
	LKNC float64 `json:"lknc"`
	RKNX float64 `json:"rknx"`
	RKNY float64 `json:"rkny"`
	RKNC float64 `json:"rknc"`
}

// ppWindow 一窗的关键点序列（缓存 JSON 的最小单元）。
type ppWindow struct {
	Clip   string    `json:"clip"`
	Sec    int       `json:"sec"`
	Label  string    `json:"label"`
	Frames []ppFrame `json:"frames"`
}

// ppSel 采样选中的窗（推理输入）。
type ppSel struct {
	Clip  string
	Sec   int
	Label string
}

// ppRow 一窗的全部读数（特征 + 门统计），随结果 JSON 落盘供复分析。
type ppRow struct {
	Clip      string  `json:"clip"`
	Sec       int     `json:"sec"`
	Label     string  `json:"label"`
	DetRate   float64 `json:"det_rate"`
	VisMean   float64 `json:"vis_mean"`
	FaceMean  float64 `json:"face_mean"`
	GateDance bool    `json:"gate_dance"` // 现场门参数 vis0.6/face0.12/det0.3 的判舞
	// 特征值 NaN 序列化为 null
	LimbSpeed       *float64 `json:"limb_speed"`
	TorsoSpeed      *float64 `json:"torso_speed"`
	TorsoRatio      *float64 `json:"torso_ratio"`
	WristAnkleRatio *float64 `json:"wrist_ankle_ratio"`
	AnkleVis        *float64 `json:"ankle_vis"`
	SpeedVar        *float64 `json:"speed_var"`
	VertRatio       *float64 `json:"vert_ratio"`
	DirReversal     *float64 `json:"dir_reversal"`
}

// ppFeatureEval 单特征评估：对各类的 AUC（正类=dance）、类均值、覆盖率。
type ppFeatureEval struct {
	AUC       map[string]*float64 `json:"auc"`
	ClassMean map[string]*float64 `json:"class_mean"`
	Coverage  map[string]int      `json:"coverage"`
}

// ppVetoOut 一条否决规则的模拟结果（只作用于门判舞窗；特征缺失不否决）。
type ppVetoOut struct {
	Rule           string `json:"rule"`
	GestureBefore  int    `json:"gesture_before"`
	GestureAfter   int    `json:"gesture_after"`
	GestureTotal   int    `json:"gesture_total"`
	DanceGateDance int    `json:"dance_gate_dance"` // 采样 dance 窗中门判舞数
	DanceKill      int    `json:"dance_kill"`       // 其中被否决数（误杀估计）
	CloseupFreed   int    `json:"closeup_freed"`    // closeup 采样中门判舞被否决（额外收益）
	ChatFreed      int    `json:"chat_freed"`
}

// ppResult 结果文档（含逐窗 rows 供复分析，不必重推理）。
type ppResult struct {
	GeneratedAt      string                    `json:"generated_at"`
	Params           map[string]interface{}    `json:"params"`
	Windows          int                       `json:"windows"`
	WindowsEffective int                       `json:"windows_effective"`
	Features         map[string]*ppFeatureEval `json:"features"`
	Veto             []*ppVetoOut              `json:"veto_simulation"`
	Rows             []ppRow                   `json:"rows"`
}

func cmdPoseProbe(args []string) {
	fs := flag.NewFlagSet("pose-probe", flag.ExitOnError)
	goldPath := fs.String("gold", "D:/upload/_diag/train/_pose_pilot/gold_review.json", "金标窗级标签 JSON")
	framesRoot := fs.String("frames", "D:/upload/_diag/train/_pose_pilot/frames", "1fps 帧根目录（子目录=片）")
	posePath := fs.String("pose", "D:/upload/_diag/train/pose_features_go.json", "每秒姿态特征 JSON（门统计用）")
	cachePath := fs.String("cache", "D:/upload/_diag/train/_pose_probe/kpt_cache.json", "关键点缓存 JSON（增量续跑）")
	outPath := fs.String("out", "D:/upload/_diag/train/pose_probe_result.json", "结果 JSON 输出路径")
	dllPath := fs.String("dll", "onnxruntime.dll", "onnxruntime.dll 路径")
	modelPath := fs.String("model", "yolov8n-pose.onnx", "姿态 ONNX 模型路径")
	nDance := fs.Int("n-dance", 250, "dance 窗采样数（0=全取）")
	nCloseup := fs.Int("n-closeup", 150, "closeup 窗采样数（0=全取）")
	nChat := fs.Int("n-chat", 150, "chat 窗采样数（0=全取）")
	seed := fs.Int64("seed", 42, "采样随机种子（确定性复现）")
	limit := fs.Int("limit", 0, "本次最多推理窗数（0=不限，冒烟用）")
	fs.Parse(args)

	gold := map[string]map[string]string{}
	if b, err := os.ReadFile(*goldPath); err != nil {
		fmt.Fprintf(os.Stderr, "读金标失败 %s: %v\n", *goldPath, err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &gold); err != nil {
		fmt.Fprintf(os.Stderr, "解析金标失败: %v\n", err)
		os.Exit(1)
	}
	poseFeats := map[string]agClipFeats{}
	if b, err := os.ReadFile(*posePath); err != nil {
		fmt.Fprintf(os.Stderr, "读姿态特征失败 %s: %v\n", *posePath, err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &poseFeats); err != nil {
		fmt.Fprintf(os.Stderr, "解析姿态特征失败: %v\n", err)
		os.Exit(1)
	}

	sel := ppSelectWindows(gold, *nDance, *nCloseup, *nChat, *seed)
	byLabel := map[string]int{}
	for _, s := range sel {
		byLabel[s.Label]++
	}
	fmt.Printf("选中窗: %d %v\n", len(sel), ppSortedCounts(byLabel))

	// 缓存增量：已推理窗直接复用，断点续跑
	cache := map[string]ppWindow{}
	if b, err := os.ReadFile(*cachePath); err == nil {
		_ = json.Unmarshal(b, &cache)
	}
	todo := make([]ppSel, 0, len(sel))
	for _, s := range sel {
		if _, ok := cache[ppKey(s.Clip, s.Sec)]; !ok {
			todo = append(todo, s)
		}
	}
	fmt.Printf("缓存命中: %d，待推理: %d\n", len(sel)-len(todo), len(todo))

	if len(todo) > 0 {
		det, err := pose.NewDetector(*dllPath, *modelPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "姿态初始化失败:", err)
			os.Exit(1)
		}
		defer func() { _ = det.Close() }()
		n := len(todo)
		if *limit > 0 && *limit < n {
			n = *limit
		}
		for i, s := range todo[:n] {
			fr, nf := ppInferWindow(det, filepath.Join(*framesRoot, s.Clip), s.Sec)
			cache[ppKey(s.Clip, s.Sec)] = ppWindow{Clip: s.Clip, Sec: s.Sec, Label: s.Label, Frames: fr}
			fmt.Printf("  [%d/%d] %s@%ds %s: %d/%d 帧有效\n", i+1, n, trunc(s.Clip, 36), s.Sec, s.Label, nf, ppWinSec)
			if (i+1)%10 == 0 || i+1 == n {
				ppSaveCache(*cachePath, cache)
			}
		}
		ppSaveCache(*cachePath, cache)
	}

	rows := ppBuildRows(sel, cache, poseFeats)
	params := map[string]interface{}{
		"gold": *goldPath, "n_dance": *nDance, "n_closeup": *nCloseup,
		"n_chat": *nChat, "seed": *seed,
	}
	res := ppEvaluate(rows, params)
	ppPrintReport(res)

	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化结果失败: %v\n", err)
		os.Exit(1)
	}
	tmp := *outPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写结果失败: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "替换结果失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("pose-probe 完成 → %s\n", *outPath)
}

func ppKey(clip string, sec int) string { return clip + "#" + strconv.Itoa(sec) }

func ppSaveCache(path string, cache map[string]ppWindow) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(cache)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// ppSelectWindows 按类选窗：gesture/none/other/gift 全取（样本小或关键对照），
// dance/closeup/chat 超 n 时按种子确定性采样；结果按片名+秒排序（同片帧连续读盘）。
func ppSelectWindows(gold map[string]map[string]string, nDance, nCloseup, nChat int, seed int64) []ppSel {
	byLabel := map[string][]ppSel{}
	clips := make([]string, 0, len(gold))
	for c := range gold {
		clips = append(clips, c)
	}
	sort.Strings(clips)
	for _, clip := range clips {
		wins := make([]int, 0, len(gold[clip]))
		for k := range gold[clip] {
			if s, err := strconv.Atoi(k); err == nil {
				wins = append(wins, s)
			}
		}
		sort.Ints(wins)
		for _, s := range wins {
			lb := gold[clip][strconv.Itoa(s)]
			byLabel[lb] = append(byLabel[lb], ppSel{Clip: clip, Sec: s, Label: lb})
		}
	}
	rng := rand.New(rand.NewSource(seed))
	sampled := map[string]int{"dance": nDance, "closeup": nCloseup, "chat": nChat}
	sel := make([]ppSel, 0, 1024)
	for _, label := range ppSortedKeys(byLabel) {
		list := byLabel[label]
		if n, ok := sampled[label]; ok && n > 0 && len(list) > n {
			rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
			list = list[:n]
		}
		sel = append(sel, list...)
	}
	sort.Slice(sel, func(i, j int) bool {
		if sel[i].Clip != sel[j].Clip {
			return sel[i].Clip < sel[j].Clip
		}
		return sel[i].Sec < sel[j].Sec
	})
	return sel
}

// ppInferWindow 推理一窗：秒 s..s+7 ↔ 帧 f_%04d(s+1)（review-ingest fps=1 抽帧同序）。
// 返回有效帧序列（躯干四点 conf 达标且躯干长非退化）与有效帧数。
func ppInferWindow(det *pose.Detector, fdir string, sec int) ([]ppFrame, int) {
	frames := make([]ppFrame, 0, ppWinSec)
	for i := 0; i < ppWinSec; i++ {
		s := sec + i
		img, err := loadFrameImage(filepath.Join(fdir, fmt.Sprintf("f_%04d.jpg", s+1)))
		if err != nil {
			continue
		}
		fp, err := det.DetectImage(img)
		if err != nil || fp == nil || !fp.Detected {
			continue
		}
		k := fp.Kpts
		mid := func(a, b pose.Landmark) (float64, float64) { return (a.X + b.X) / 2, (a.Y + b.Y) / 2 }
		if k[5].Conf < ppTorsoConf || k[6].Conf < ppTorsoConf || k[11].Conf < ppTorsoConf || k[12].Conf < ppTorsoConf {
			continue
		}
		msx, msy := mid(k[5], k[6])
		mhx, mhy := mid(k[11], k[12])
		if math.Hypot(msx-mhx, msy-mhy) < 1 {
			continue
		}
		frames = append(frames, ppFrame{
			Sec: float64(s),
			MSX: msx, MSY: msy, MHX: mhx, MHY: mhy,
			LWX: k[9].X, LWY: k[9].Y, LWC: k[9].Conf,
			RWX: k[10].X, RWY: k[10].Y, RWC: k[10].Conf,
			LAX: k[15].X, LAY: k[15].Y, LAC: k[15].Conf,
			RAX: k[16].X, RAY: k[16].Y, RAC: k[16].Conf,
			NX: k[0].X, NY: k[0].Y, NC: k[0].Conf,
			LEX: k[1].X, LEY: k[1].Y, LEC: k[1].Conf,
			REX: k[2].X, REY: k[2].Y, REC: k[2].Conf,
			LARX: k[3].X, LARY: k[3].Y, LARC: k[3].Conf,
			RARX: k[4].X, RARY: k[4].Y, RARC: k[4].Conf,
			LSX: k[5].X, LSY: k[5].Y, LSC: k[5].Conf,
			RSX: k[6].X, RSY: k[6].Y, RSC: k[6].Conf,
			LELX: k[7].X, LELY: k[7].Y, LELC: k[7].Conf,
			RELX: k[8].X, RELY: k[8].Y, RELC: k[8].Conf,
			LKNX: k[13].X, LKNY: k[13].Y, LKNC: k[13].Conf,
			RKNX: k[14].X, RKNY: k[14].Y, RKNC: k[14].Conf,
		})
	}
	return frames, len(frames)
}

// ppBuildRows 缓存窗 × 特征 × 门统计 → 行集合。
func ppBuildRows(sel []ppSel, cache map[string]ppWindow, poseFeats map[string]agClipFeats) []ppRow {
	rows := make([]ppRow, 0, len(sel))
	for _, s := range sel {
		w, ok := cache[ppKey(s.Clip, s.Sec)]
		if !ok {
			continue
		}
		row := ppRow{Clip: w.Clip, Sec: w.Sec, Label: w.Label}
		if cf, ok := poseFeats[s.Clip]; ok && s.Sec < len(cf.Feats) {
			e := s.Sec + ppWinSec
			if e > len(cf.Feats) {
				e = len(cf.Feats)
			}
			win := make([]pose.FrameFeatures, e-s.Sec)
			for i, f := range cf.Feats[s.Sec:e] {
				win[i] = pose.FrameFeatures{Detected: f[4] == 1, VisRatio: f[0], FaceFrac: f[1]}
			}
			st := pose.AggregateWindow(win, 0, 0, 0)
			row.DetRate, row.VisMean, row.FaceMean = st.DetRate, st.VisMean, st.FaceMean
			row.GateDance = st.DetRate >= 0.3 && st.VisMean >= 0.6 && st.FaceMean <= 0.12
		}
		feats := ppFeatures(w)
		row.LimbSpeed = ppPtr(feats["limb_speed"])
		row.TorsoSpeed = ppPtr(feats["torso_speed"])
		row.TorsoRatio = ppPtr(feats["torso_ratio"])
		row.WristAnkleRatio = ppPtr(feats["wrist_ankle_ratio"])
		row.AnkleVis = ppPtr(feats["ankle_vis"])
		row.SpeedVar = ppPtr(feats["speed_var"])
		row.VertRatio = ppPtr(feats["vert_ratio"])
		row.DirReversal = ppPtr(feats["dir_reversal"])
		rows = append(rows, row)
	}
	return rows
}

// ppFeatures 一窗的候选特征。无效窗（帧太少）全 NaN；条件不满足的个体特征 NaN。
// 速度单位 = 躯干长/秒（每对帧位移 ÷ 间隔秒 ÷ 两帧躯干长均值）。
func ppFeatures(w ppWindow) map[string]float64 {
	out := map[string]float64{}
	for _, k := range []string{"limb_speed", "torso_speed", "torso_ratio", "wrist_ankle_ratio",
		"ankle_vis", "speed_var", "vert_ratio", "dir_reversal"} {
		out[k] = math.NaN()
	}
	fr := w.Frames
	if len(fr) < ppMinFrames {
		return out
	}
	torso := func(f ppFrame) float64 { return math.Hypot(f.MSX-f.MHX, f.MSY-f.MHY) }
	type ppPt struct{ x, y, c float64 }
	points := func(f ppFrame) []ppPt {
		return []ppPt{{f.LWX, f.LWY, f.LWC}, {f.RWX, f.RWY, f.RWC}, {f.LAX, f.LAY, f.LAC}, {f.RAX, f.RAY, f.RAC}}
	}
	limbSpeeds := []float64{} // 每对帧：公共可见腕踝点的平均速度
	vertDx, vertDy := 0.0, 0.0
	torsoSpeeds := []float64{}
	for i := 1; i < len(fr); i++ {
		f, p := fr[i], fr[i-1]
		gap := f.Sec - p.Sec
		if gap <= 0 || gap > ppMaxGapSec {
			continue
		}
		norm := (torso(f) + torso(p)) / 2
		if norm < 1 {
			continue
		}
		pa, pb := points(p), points(f)
		sp, n := 0.0, 0
		for j := range pa {
			if pa[j].c >= ppConf && pb[j].c >= ppConf {
				sp += math.Hypot(pb[j].x-pa[j].x, pb[j].y-pa[j].y) / gap / norm
				vertDx += math.Abs(pb[j].x - pa[j].x)
				vertDy += math.Abs(pb[j].y - pa[j].y)
				n++
			}
		}
		if n > 0 {
			limbSpeeds = append(limbSpeeds, sp/float64(n))
		}
		ts := (math.Hypot(f.MSX-p.MSX, f.MSY-p.MSY) + math.Hypot(f.MHX-p.MHX, f.MHY-p.MHY)) / 2 / gap / norm
		torsoSpeeds = append(torsoSpeeds, ts)
	}
	ls, ts := ppMean(limbSpeeds), ppMean(torsoSpeeds)
	out["limb_speed"], out["torso_speed"] = ls, ts
	if !math.IsNaN(ls) {
		out["torso_ratio"] = ts / (ls + ppTorsoEps)
		var2 := 0.0
		for _, x := range limbSpeeds {
			var2 += (x - ls) * (x - ls)
		}
		out["speed_var"] = var2 / float64(len(limbSpeeds))
		if vertDx+vertDy > 0 {
			out["vert_ratio"] = vertDy / (vertDx + vertDy)
		}
	}
	// 双踝可见帧占比（构图特征：半身=踝出画）
	nAnkle := 0
	for _, f := range fr {
		if f.LAC >= ppConf && f.RAC >= ppConf {
			nAnkle++
		}
	}
	ankleVis := float64(nAnkle) / float64(len(fr))
	out["ankle_vis"] = ankleVis
	// 腕速/踝速比：要求踝部数据足够（双踝可见帧占比达标），否则 NaN
	if ankleVis >= ppAnkleVisMin {
		wristSpeeds, ankleSpeeds := []float64{}, []float64{}
		for i := 1; i < len(fr); i++ {
			f, p := fr[i], fr[i-1]
			gap := f.Sec - p.Sec
			if gap <= 0 || gap > ppMaxGapSec {
				continue
			}
			norm := (torso(f) + torso(p)) / 2
			if norm < 1 {
				continue
			}
			speed := func(ax, ay, ac, bx, by, bc float64) (float64, bool) {
				if ac >= ppConf && bc >= ppConf {
					return math.Hypot(bx-ax, by-ay) / gap / norm, true
				}
				return 0, false
			}
			if v, ok := speed(p.LWX, p.LWY, p.LWC, f.LWX, f.LWY, f.LWC); ok {
				wristSpeeds = append(wristSpeeds, v)
			}
			if v, ok := speed(p.RWX, p.RWY, p.RWC, f.RWX, f.RWY, f.RWC); ok {
				wristSpeeds = append(wristSpeeds, v)
			}
			if v, ok := speed(p.LAX, p.LAY, p.LAC, f.LAX, f.LAY, f.LAC); ok {
				ankleSpeeds = append(ankleSpeeds, v)
			}
			if v, ok := speed(p.RAX, p.RAY, p.RAC, f.RAX, f.RAY, f.RAC); ok {
				ankleSpeeds = append(ankleSpeeds, v)
			}
		}
		ws, as := ppMean(wristSpeeds), ppMean(ankleSpeeds)
		// as 下限防「踝全静止 → 比值爆炸」；0.02 躯干长/秒 ≈ 近静止
		if !math.IsNaN(ws) && !math.IsNaN(as) && as > 0.02 {
			out["wrist_ankle_ratio"] = ws / as
		}
	}
	out["dir_reversal"] = ppDirReversal(fr, torso)
	return out
}

// ppDirReversal 腕部（左右各自）位移方向反转度：同侧腕连续三帧构成两段位移向量，
// 取 1-cosθ（0=同向 2=全反向），两段各自归一化躯干长需达 ppMinSeg（滤静止噪声方向）。
// 可用三元组 <2 组 → NaN。
func ppDirReversal(fr []ppFrame, torso func(ppFrame) float64) float64 {
	vals := []float64{}
	for side := 0; side < 2; side++ {
		type pt struct {
			sec, x, y float64
		}
		seq := make([]pt, 0, len(fr))
		for _, f := range fr {
			c, x, y := f.LWC, f.LWX, f.LWY
			if side == 1 {
				c, x, y = f.RWC, f.RWX, f.RWY
			}
			if c >= ppConf {
				seq = append(seq, pt{f.Sec, x / torso(f), y / torso(f)})
			}
		}
		for i := 2; i < len(seq); i++ {
			g1 := seq[i-1].sec - seq[i-2].sec
			g2 := seq[i].sec - seq[i-1].sec
			if g1 <= 0 || g1 > ppMaxGapSec || g2 <= 0 || g2 > ppMaxGapSec {
				continue
			}
			a1x, a1y := seq[i-1].x-seq[i-2].x, seq[i-1].y-seq[i-2].y
			a2x, a2y := seq[i].x-seq[i-1].x, seq[i].y-seq[i-1].y
			n1, n2 := math.Hypot(a1x, a1y), math.Hypot(a2x, a2y)
			if n1 < ppMinSeg || n2 < ppMinSeg {
				continue
			}
			vals = append(vals, 1-(a1x*a2x+a1y*a2y)/(n1*n2))
		}
	}
	if len(vals) < 2 {
		return math.NaN()
	}
	s := 0.0
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals))
}

func ppMean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func ppPtr(f float64) *float64 {
	if math.IsNaN(f) {
		return nil
	}
	return &f
}

// ppAUC Mann-Whitney AUC（正类得分应更高）；NaN 值剔除，返回有效样本数。
func ppAUC(pos, neg []float64) (auc float64, nPos, nNeg int) {
	p := ppFilterNaN(pos)
	n := ppFilterNaN(neg)
	if len(p) == 0 || len(n) == 0 {
		return math.NaN(), len(p), len(n)
	}
	all := append(append([]float64{}, p...), n...)
	sort.Float64s(all)
	// 正类秩和（平均秩处理并列），AUC = (R - n1(n1+1)/2) / (n1*n2)
	rankSum := 0.0
	for _, v := range p {
		l := sort.SearchFloat64s(all, v)
		r := l
		for r < len(all) && all[r] == v {
			r++
		}
		rankSum += float64(l+1+r) / 2 // 秩 l+1..r（1 起）的平均
	}
	auc = (rankSum - float64(len(p))*float64(len(p)+1)/2) / float64(len(p)*len(n))
	return auc, len(p), len(n)
}

func ppFilterNaN(xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for _, x := range xs {
		if !math.IsNaN(x) {
			out = append(out, x)
		}
	}
	return out
}

// ppGet 行特征取值（nil=NaN），供评估汇总统一访问。
func ppGet(r *ppRow, name string) *float64 {
	switch name {
	case "limb_speed":
		return r.LimbSpeed
	case "torso_speed":
		return r.TorsoSpeed
	case "torso_ratio":
		return r.TorsoRatio
	case "wrist_ankle_ratio":
		return r.WristAnkleRatio
	case "ankle_vis":
		return r.AnkleVis
	case "speed_var":
		return r.SpeedVar
	case "vert_ratio":
		return r.VertRatio
	case "dir_reversal":
		return r.DirReversal
	}
	return nil
}

var ppFeatureNames = []string{"limb_speed", "torso_speed", "torso_ratio", "wrist_ankle_ratio",
	"ankle_vis", "speed_var", "vert_ratio", "dir_reversal"}

// ppEvaluate 汇总：逐特征 AUC（正类=dance，vs 各类）+ 类均值 + 否决规则模拟。
func ppEvaluate(rows []ppRow, params map[string]interface{}) *ppResult {
	// 类别顺序固定输出：主对照 gesture 在前
	classOrder := []string{"gesture", "dance", "closeup", "chat", "none", "other", "gift"}
	inOrder := map[string]int{}
	for i, c := range classOrder {
		inOrder[c] = i
	}
	byClass := map[string][]*ppRow{}
	for i := range rows {
		r := &rows[i]
		byClass[r.Label] = append(byClass[r.Label], r)
	}
	effective := 0
	for _, r := range rows {
		if r.LimbSpeed != nil { // 有效窗标志（非全 NaN）
			effective++
		}
	}
	feats := map[string]*ppFeatureEval{}
	for _, name := range ppFeatureNames {
		eval := &ppFeatureEval{AUC: map[string]*float64{}, ClassMean: map[string]*float64{}, Coverage: map[string]int{}}
		vals := map[string][]float64{}
		for _, c := range classOrder {
			for _, r := range byClass[c] {
				if v := ppGet(r, name); v != nil {
					vals[c] = append(vals[c], *v)
				}
			}
		}
		for _, c := range classOrder {
			if c == "dance" || len(vals[c]) == 0 {
				continue
			}
			auc, np, nn := ppAUC(vals["dance"], vals[c])
			if nn > 0 && np > 0 {
				eval.AUC["vs_"+c] = ppPtr(auc)
				eval.Coverage["vs_"+c] = nn
			}
		}
		for _, c := range classOrder {
			if len(vals[c]) == 0 {
				continue
			}
			eval.ClassMean[c] = ppPtr(ppMean(vals[c]))
			eval.Coverage[c] = len(vals[c])
		}
		feats[name] = eval
	}

	res := &ppResult{
		GeneratedAt:      time.Now().Format("2006/1/2 15:04:05"),
		Params:           params,
		Windows:          len(rows),
		WindowsEffective: effective,
		Features:         feats,
		Veto:             ppVetoSimulation(rows, byClass, inOrder),
		Rows:             rows,
	}
	return res
}

// ppVetoSimulation 模拟「现有门 ∧ 特征否决」：基线=现场门参数判舞；
// 否决只在特征有值且条件成立时触发（缺失不惩罚）。gesture 全量精确，
// dance/closeup/chat 为采样估计。
func ppVetoSimulation(rows []ppRow, byClass map[string][]*ppRow, inOrder map[string]int) []*ppVetoOut {
	type rule struct {
		name string
		fire func(r *ppRow) bool
	}
	rules := []rule{
		{"ankle_vis<0.25", func(r *ppRow) bool { return r.AnkleVis != nil && *r.AnkleVis < 0.25 }},
		{"ankle_vis<0.5", func(r *ppRow) bool { return r.AnkleVis != nil && *r.AnkleVis < 0.5 }},
		{"torso_ratio<0.3", func(r *ppRow) bool { return r.TorsoRatio != nil && *r.TorsoRatio < 0.3 }},
		{"torso_ratio<0.4", func(r *ppRow) bool { return r.TorsoRatio != nil && *r.TorsoRatio < 0.4 }},
		{"wrist_ankle_ratio>2", func(r *ppRow) bool { return r.WristAnkleRatio != nil && *r.WristAnkleRatio > 2 }},
		{"wrist_ankle_ratio>3", func(r *ppRow) bool { return r.WristAnkleRatio != nil && *r.WristAnkleRatio > 3 }},
		{"any(av<0.25,tr<0.3)", func(r *ppRow) bool {
			return (r.AnkleVis != nil && *r.AnkleVis < 0.25) || (r.TorsoRatio != nil && *r.TorsoRatio < 0.3)
		}},
		{"any(av<0.25,tr<0.3,war>2)", func(r *ppRow) bool {
			return (r.AnkleVis != nil && *r.AnkleVis < 0.25) || (r.TorsoRatio != nil && *r.TorsoRatio < 0.3) ||
				(r.WristAnkleRatio != nil && *r.WristAnkleRatio > 2)
		}},
	}
	// 基线：各类门判舞计数
	gestTotal, gestBefore := 0, 0
	for _, r := range byClass["gesture"] {
		gestTotal++
		if r.GateDance {
			gestBefore++
		}
	}
	danceGate := 0
	for _, r := range byClass["dance"] {
		if r.GateDance {
			danceGate++
		}
	}
	out := make([]*ppVetoOut, 0, len(rules))
	for _, ru := range rules {
		v := &ppVetoOut{
			Rule: ru.name, GestureBefore: gestBefore, GestureTotal: gestTotal,
			DanceGateDance: danceGate,
		}
		firedG := 0
		for _, r := range byClass["gesture"] {
			if r.GateDance && ru.fire(r) {
				firedG++
			}
		}
		v.GestureAfter = v.GestureBefore - firedG // 剩余判舞数（曾误存被否决数导致语义反转）
		for _, r := range byClass["dance"] {
			if r.GateDance && ru.fire(r) {
				v.DanceKill++
			}
		}
		for _, r := range byClass["closeup"] {
			if r.GateDance && ru.fire(r) {
				v.CloseupFreed++
			}
		}
		for _, r := range byClass["chat"] {
			if r.GateDance && ru.fire(r) {
				v.ChatFreed++
			}
		}
		out = append(out, v)
	}
	return out
}

func ppPrintReport(res *ppResult) {
	fmt.Printf("\n有效窗: %d/%d\n", res.WindowsEffective, res.Windows)
	fmt.Printf("\n逐特征 AUC（正类=dance，得分越高越像舞）:\n")
	fmt.Printf("  %-18s %10s %10s %10s %10s | 覆盖 g/d\n", "feature", "vs_gesture", "vs_closeup", "vs_chat", "vs_none")
	for _, name := range ppFeatureNames {
		e := res.Features[name]
		cell := func(k string) string {
			if v, ok := e.AUC[k]; ok && v != nil {
				return fmt.Sprintf("%.3f", *v)
			}
			return "—"
		}
		fmt.Printf("  %-18s %10s %10s %10s %10s | %d/%d\n", name,
			cell("vs_gesture"), cell("vs_closeup"), cell("vs_chat"), cell("vs_none"),
			e.Coverage["gesture"], e.Coverage["dance"])
	}
	fmt.Printf("\n类均值（判读方向用）:\n")
	for _, name := range ppFeatureNames {
		e := res.Features[name]
		cell := func(c string) string {
			if v, ok := e.ClassMean[c]; ok && v != nil {
				return fmt.Sprintf("%.3f", *v)
			}
			return "—"
		}
		fmt.Printf("  %-18s gesture=%-9s dance=%-9s closeup=%-9s chat=%-9s none=%s\n",
			name, cell("gesture"), cell("dance"), cell("closeup"), cell("chat"), cell("none"))
	}
	fmt.Printf("\n否决规则模拟（门判舞 ∧ 特征否决；gesture 全量，dance/closeup/chat 为采样估计）:\n")
	for _, v := range res.Veto {
		afterPct, beforePct := 0.0, 0.0
		if v.GestureTotal > 0 {
			afterPct = 100 * float64(v.GestureAfter) / float64(v.GestureTotal)
			beforePct = 100 * float64(v.GestureBefore) / float64(v.GestureTotal)
		}
		killPct := 0.0
		if v.DanceGateDance > 0 {
			killPct = 100 * float64(v.DanceKill) / float64(v.DanceGateDance)
		}
		fmt.Printf("  %-26s gesture 判舞 %d/%d(%.1f%%)→%d(%.1f%%) | 舞误杀 %d/%d(%.1f%%) | 释放 closeup %d chat %d\n",
			v.Rule, v.GestureBefore, v.GestureTotal, beforePct, v.GestureAfter, afterPct,
			v.DanceKill, v.DanceGateDance, killPct, v.CloseupFreed, v.ChatFreed)
	}
}

func ppSortedKeys[T any](m map[string][]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func ppSortedCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}
