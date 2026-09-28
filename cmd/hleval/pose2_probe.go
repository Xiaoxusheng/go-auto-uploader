// pose2-probe 姿态构形特征探针（二代姿态特征，§29）。
//
// 背景：§26 只测了「位移/速度」族（腕踝+躯干中点 8 点），「构形/几何」族未测——
// 手到脸距离、腕高度、肩线朝向、肘角、头部稳定度等直接编码「讲话手势 vs 编舞」语义，
// 且全部来自已部署的 yolov8n-pose 17 关键点，零新增依赖（CLIP 方案已被用户否决）。
//
// 数据：audio_probe_gold.json（330 窗 AI 盲标，§27 建）× frames480/（480p 1fps，与生产口径一致）。
// 判定线（预注册）：按主播留出 AUC ≥0.85 → 主链路候选；<0.70 → 关死。
//
// 用法：
//
//	hleval pose2-probe [-gold <json>] [-frames <dir>] [-cache <json>] [-out <json>]
//	                  [-dll ...] [-model ...] [-limit N]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"upload/internal/pose"
)

const (
	p2Conf   = 0.5 // 腕/脸/肘点可见阈值
	p2FaceR  = 0.6 // 手-脸接触半径（肩宽倍数）
	p2MinFr  = 4   // 有效帧下限
	p2MaxGap = 3   // 位移对最大间隔秒
	// p2CoreConf §30 可见性统计阈值：刻意取 0.3，与生产 FeaturesFromFrame 的
	// `k.Conf < 0.3 { continue }` 同口径，保证探针结论可迁移。
	p2CoreConf = 0.3
)

// pp2Feats 一窗构形特征（NaN=不可判）。
type pp2Feats struct {
	FaceTouch     float64 `json:"face_touch"`
	FaceDistMean  float64 `json:"face_dist_mean"`
	WristHigh     float64 `json:"wrist_high"`
	WristSpanMean float64 `json:"wrist_span_mean"`
	WristSpanVar  float64 `json:"wrist_span_var"`
	ShoAngleStd   float64 `json:"sho_angle_std"`
	LeanMean      float64 `json:"lean_mean"`
	ElbowAngMean  float64 `json:"elbow_ang_mean"`
	ElbowAngStd   float64 `json:"elbow_ang_std"`
	ElbowHipDist  float64 `json:"elbow_hip_dist"`
	NoseSpeed     float64 `json:"nose_speed"`
	NoseXStd      float64 `json:"nose_x_std"`
	KneeVis       float64 `json:"knee_vis"`
	ArmAsym       float64 `json:"arm_asym"`
	// §30 新增：可见点**数量**（生产 FeaturesFromFrame.VisRatio 只算可见点的平均置信度，
	// 近景与全身舞都高 → 无法区分。这两项补上「可见性」这一维）。
	CoreVis float64 `json:"core_vis"` // 肩/腕/踝 6 点可见(conf≥0.3)的平均占比
	LegVis  float64 `json:"leg_vis"`  // 膝+踝 4 点可见的平均占比（近景下肢出画）
}

func (f pp2Feats) get(name string) float64 {
	switch name {
	case "face_touch":
		return f.FaceTouch
	case "face_dist_mean":
		return f.FaceDistMean
	case "wrist_high":
		return f.WristHigh
	case "wrist_span_mean":
		return f.WristSpanMean
	case "wrist_span_var":
		return f.WristSpanVar
	case "sho_angle_std":
		return f.ShoAngleStd
	case "lean_mean":
		return f.LeanMean
	case "elbow_ang_mean":
		return f.ElbowAngMean
	case "elbow_ang_std":
		return f.ElbowAngStd
	case "elbow_hip_dist":
		return f.ElbowHipDist
	case "nose_speed":
		return f.NoseSpeed
	case "nose_x_std":
		return f.NoseXStd
	case "knee_vis":
		return f.KneeVis
	case "arm_asym":
		return f.ArmAsym
	case "core_vis":
		return f.CoreVis
	case "leg_vis":
		return f.LegVis
	}
	return math.NaN()
}

var pp2Names = []string{"face_touch", "face_dist_mean", "wrist_high", "wrist_span_mean",
	"wrist_span_var", "sho_angle_std", "lean_mean", "elbow_ang_mean", "elbow_ang_std",
	"elbow_hip_dist", "nose_speed", "nose_x_std", "knee_vis", "arm_asym",
	"core_vis", "leg_vis"}

// pp2Row 一窗读数。
type pp2Row struct {
	Clip  string              `json:"clip"`
	Sec   int                 `json:"sec"`
	Label string              `json:"label"`
	Feats map[string]*float64 `json:"feats"`
}

func (r pp2Row) feat(name string) *float64 { return r.Feats[name] }

type pp2FeatureEval struct {
	AUC       map[string]*float64 `json:"auc"`
	ClassMean map[string]*float64 `json:"class_mean"`
	Coverage  map[string]int      `json:"coverage"`
}

type pp2Result struct {
	GeneratedAt string                     `json:"generated_at"`
	Windows     int                        `json:"windows"`
	Features    map[string]*pp2FeatureEval `json:"features"`
	Rows        []pp2Row                   `json:"rows"`
}

func cmdPose2Probe(args []string) {
	fs := flag.NewFlagSet("pose2-probe", flag.ExitOnError)
	goldPath := fs.String("gold", "D:/upload/_diag/train/audio_probe/audio_probe_gold.json", "金标窗级标签 JSON")
	framesRoot := fs.String("frames", "D:/upload/_diag/train/audio_probe/frames480", "480p 1fps 帧根目录")
	cachePath := fs.String("cache", "D:/upload/_diag/train/audio_probe/kpt_cache2.json", "全关键点缓存 JSON（增量续跑）")
	outPath := fs.String("out", "D:/upload/_diag/train/pose2_probe_result.json", "结果 JSON")
	dllPath := fs.String("dll", "onnxruntime.dll", "onnxruntime.dll 路径")
	modelPath := fs.String("model", "yolov8n-pose.onnx", "姿态 ONNX 模型路径")
	limit := fs.Int("limit", 0, "本次最多推理窗数（0=不限）")
	fs.Parse(args)

	gold := map[string]map[string]string{}
	if b, err := os.ReadFile(*goldPath); err != nil {
		fmt.Fprintf(os.Stderr, "读金标失败: %v\n", err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &gold); err != nil {
		fmt.Fprintf(os.Stderr, "解析金标失败: %v\n", err)
		os.Exit(1)
	}

	sel := ppSelectWindows(gold, 0, 0, 0, 42) // 0=各类全取
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
	fmt.Printf("选中窗: %d，缓存命中: %d，待推理: %d\n", len(sel), len(sel)-len(todo), len(todo))

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
			if (i+1)%20 == 0 || i+1 == n {
				ppSaveCache(*cachePath, cache)
			}
		}
		ppSaveCache(*cachePath, cache)
	}

	rows := make([]pp2Row, 0, len(sel))
	for _, s := range sel {
		w, ok := cache[ppKey(s.Clip, s.Sec)]
		if !ok {
			continue
		}
		feats := pp2Features(w)
		m := make(map[string]*float64, len(pp2Names))
		for _, name := range pp2Names {
			m[name] = ppPtr(feats.get(name))
		}
		rows = append(rows, pp2Row{Clip: w.Clip, Sec: w.Sec, Label: w.Label, Feats: m})
	}
	res := pp2Evaluate(rows)
	pp2PrintReport(res)

	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化失败: %v\n", err)
		os.Exit(1)
	}
	tmp := *outPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写结果失败: %v\n", err)
		os.Exit(1)
	}
	_ = os.Rename(tmp, *outPath)
	fmt.Printf("pose2-probe 完成 → %s\n", *outPath)
}

// pp2Features 构形特征：距离按肩宽 |Lsho-Rsho| 归一化，速度按躯干长/秒。
// 仅统计肩部可见（肩宽≥1px）的帧；脸部点/腕分别按 conf 门槛参与。
func pp2Features(w ppWindow) pp2Feats {
	nan := math.NaN()
	out := pp2Feats{FaceTouch: nan, FaceDistMean: nan, WristHigh: nan, WristSpanMean: nan,
		WristSpanVar: nan, ShoAngleStd: nan, LeanMean: nan, ElbowAngMean: nan,
		ElbowAngStd: nan, ElbowHipDist: nan, NoseSpeed: nan, NoseXStd: nan,
		KneeVis: nan, ArmAsym: nan, CoreVis: nan, LegVis: nan}
	fr := w.Frames
	// §30 可见性特征不依赖帧数下限：近景窗常常有效帧很少（下肢出画），
	// 若等下面的 p2MinFr 检查之后再算，最需要它的窗反而永远拿不到值。
	if len(fr) > 0 {
		var cs, ls float64
		for i := range fr {
			f := fr[i]
			cv, lv := 0.0, 0.0
			for _, c := range []float64{f.LSC, f.RSC, f.LWC, f.RWC, f.LAC, f.RAC} {
				if c >= p2CoreConf {
					cv++
				}
			}
			for _, c := range []float64{f.LAC, f.RAC, f.LKNC, f.RKNC} {
				if c >= p2CoreConf {
					lv++
				}
			}
			cs += cv / 6
			ls += lv / 4
		}
		out.CoreVis = cs / float64(len(fr))
		out.LegVis = ls / float64(len(fr))
	}
	if len(fr) < p2MinFr {
		return out
	}
	torso := func(f ppFrame) float64 { return math.Max(math.Hypot(f.MSX-f.MHX, f.MSY-f.MHY), 1) }

	var nValid, nHigh, nKnee int
	faceDists, spans, angles, leans := []float64{}, []float64{}, []float64{}, []float64{}
	elbowAngs, elbowHips := []float64{}, []float64{}
	noseXs, noseSpeeds := []float64{}, []float64{}
	var lSpeeds, rSpeeds []float64

	for i := range fr {
		f := fr[i]
		sw := 0.0
		if f.LSC >= p2Conf && f.RSC >= p2Conf {
			sw = math.Hypot(f.RSX-f.LSX, f.RSY-f.LSY)
		}
		if sw < 1 {
			continue // 肩不可见：构形无基准
		}
		nValid++
		// 手-脸最近距离（双腕 × 脸 5 点）
		best := math.Inf(1)
		for _, wp := range [][3]float64{{f.LWX, f.LWY, f.LWC}, {f.RWX, f.RWY, f.RWC}} {
			if wp[2] < p2Conf {
				continue
			}
			for _, fp := range [][3]float64{
				{f.NX, f.NY, f.NC}, {f.LEX, f.LEY, f.LEC}, {f.REX, f.REY, f.REC},
				{f.LARX, f.LARY, f.LARC}, {f.RARX, f.RARY, f.RARC},
			} {
				if fp[2] < p2Conf {
					continue
				}
				if d := math.Hypot(wp[0]-fp[0], wp[1]-fp[1]) / sw; d < best {
					best = d
				}
			}
		}
		if !math.IsInf(best, 1) {
			faceDists = append(faceDists, best)
		}
		// 腕高于肩线（任一可见腕）
		shoTop := math.Max(f.LSY, f.RSY)
		if (f.LWC >= p2Conf && f.LWY < shoTop) || (f.RWC >= p2Conf && f.RWY < shoTop) {
			nHigh++
		}
		// 双腕开合
		if f.LWC >= p2Conf && f.RWC >= p2Conf {
			spans = append(spans, math.Hypot(f.RWX-f.LWX, f.RWY-f.LWY)/sw)
		}
		// 肩线朝角与躯干侧倾
		angles = append(angles, math.Atan2(f.RSY-f.LSY, f.RSX-f.LSX))
		leans = append(leans, math.Abs(f.MSX-f.MHX)/torso(f))
		// 头部
		if f.NC >= p2Conf {
			noseXs = append(noseXs, f.NX/sw)
		}
		if f.LKNC >= p2Conf && f.RKNC >= p2Conf {
			nKnee++
		}
		// 肘角与肘-髋距
		for _, a := range [][9]float64{
			{f.LSX, f.LSY, f.LSC, f.LELX, f.LELY, f.LELC, f.LWX, f.LWY, f.LWC},
			{f.RSX, f.RSY, f.RSC, f.RELX, f.RELY, f.RELC, f.RWX, f.RWY, f.RWC},
		} {
			if a[2] < p2Conf || a[5] < p2Conf || a[8] < p2Conf {
				continue
			}
			a1x, a1y := a[3]-a[0], a[4]-a[1]
			a2x, a2y := a[6]-a[3], a[7]-a[4]
			n1, n2 := math.Hypot(a1x, a1y), math.Hypot(a2x, a2y)
			if n1 < 1 || n2 < 1 {
				continue
			}
			elbowAngs = append(elbowAngs, math.Acos(math.Max(-1, math.Min(1, (a1x*a2x+a1y*a2y)/(n1*n2)))))
			elbowHips = append(elbowHips, math.Hypot(a[3]-f.MHX, a[4]-f.MHY)/torso(f))
		}
		// 逐秒位移（鼻/双腕）
		if i > 0 {
			p := fr[i-1]
			gap := f.Sec - p.Sec
			if gap > 0 && gap <= p2MaxGap {
				if f.NC >= p2Conf && p.NC >= p2Conf {
					noseSpeeds = append(noseSpeeds, math.Hypot(f.NX-p.NX, f.NY-p.NY)/gap/torso(f))
				}
				if f.LWC >= p2Conf && p.LWC >= p2Conf {
					lSpeeds = append(lSpeeds, math.Hypot(f.LWX-p.LWX, f.LWY-p.LWY)/gap/torso(f))
				}
				if f.RWC >= p2Conf && p.RWC >= p2Conf {
					rSpeeds = append(rSpeeds, math.Hypot(f.RWX-p.RWX, f.RWY-p.RWY)/gap/torso(f))
				}
			}
		}
	}
	if nValid < p2MinFr || len(faceDists) < p2MinFr {
		return out
	}

	touch := 0.0
	for _, d := range faceDists {
		if d < p2FaceR {
			touch++
		}
	}
	out.FaceTouch = touch / float64(len(faceDists))
	out.FaceDistMean = ppMean(faceDists)
	out.WristHigh = float64(nHigh) / float64(nValid)
	out.KneeVis = float64(nKnee) / float64(nValid)
	out.WristSpanMean = ppMean(spans)
	if len(spans) >= p2MinFr {
		m := ppMean(spans)
		v := 0.0
		for _, x := range spans {
			v += (x - m) * (x - m)
		}
		out.WristSpanVar = v / float64(len(spans))
	}
	out.ShoAngleStd = ppStd(angles)
	out.LeanMean = ppMean(leans)
	out.ElbowAngMean = ppMean(elbowAngs)
	out.ElbowAngStd = ppStd(elbowAngs)
	out.ElbowHipDist = ppMean(elbowHips)
	out.NoseSpeed = ppMean(noseSpeeds)
	out.NoseXStd = ppStd(noseXs)
	ml, mr := ppMean(lSpeeds), ppMean(rSpeeds)
	if !math.IsNaN(ml) && !math.IsNaN(mr) {
		out.ArmAsym = math.Abs(ml-mr) / (ml + mr + 1e-6)
	}
	return out
}

func ppStd(xs []float64) float64 {
	if len(xs) < 2 {
		return math.NaN()
	}
	m := ppMean(xs)
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return math.Sqrt(v / float64(len(xs)))
}

// pp2Evaluate 逐特征 AUC（正类=dance）+ 类均值。
func pp2Evaluate(rows []pp2Row) *pp2Result {
	classOrder := []string{"gesture", "dance", "closeup", "chat", "none", "other"}
	byClass := map[string][]*pp2Row{}
	for i := range rows {
		r := &rows[i]
		byClass[r.Label] = append(byClass[r.Label], r)
	}
	feats := map[string]*pp2FeatureEval{}
	for _, name := range pp2Names {
		fe := &pp2FeatureEval{AUC: map[string]*float64{}, ClassMean: map[string]*float64{}, Coverage: map[string]int{}}
		vals := map[string][]float64{}
		for _, c := range classOrder {
			for _, r := range byClass[c] {
				if v := r.feat(name); v != nil {
					vals[c] = append(vals[c], *v)
				}
			}
		}
		for _, c := range classOrder {
			if c == "dance" || len(vals[c]) == 0 {
				continue
			}
			auc, np, nn := ppAUC(vals["dance"], vals[c])
			if np > 0 && nn > 0 {
				fe.AUC["vs_"+c] = ppPtr(auc)
				fe.Coverage["vs_"+c] = nn
			}
		}
		for _, c := range classOrder {
			if len(vals[c]) == 0 {
				continue
			}
			fe.ClassMean[c] = ppPtr(ppMean(vals[c]))
			fe.Coverage[c] = len(vals[c])
		}
		feats[name] = fe
	}
	return &pp2Result{GeneratedAt: time.Now().Format("2006/1/2 15:04:05"),
		Windows: len(rows), Features: feats, Rows: rows}
}

func pp2PrintReport(res *pp2Result) {
	fmt.Printf("\n窗数: %d\n", res.Windows)
	fmt.Printf("\n构形特征 AUC（正类=dance）:\n")
	fmt.Printf("  %-16s %11s %11s %11s %11s | 覆盖 g/d\n", "feature", "vs_gesture", "vs_closeup", "vs_chat", "vs_none")
	for _, name := range pp2Names {
		e := res.Features[name]
		cell := func(k string) string {
			if v, ok := e.AUC[k]; ok && v != nil {
				return fmt.Sprintf("%.3f", *v)
			}
			return "—"
		}
		fmt.Printf("  %-16s %11s %11s %11s %11s | %d/%d\n", name,
			cell("vs_gesture"), cell("vs_closeup"), cell("vs_chat"), cell("vs_none"),
			e.Coverage["gesture"], e.Coverage["dance"])
	}
	fmt.Printf("\n类均值:\n")
	for _, name := range pp2Names {
		e := res.Features[name]
		cell := func(c string) string {
			if v, ok := e.ClassMean[c]; ok && v != nil {
				return fmt.Sprintf("%.3f", *v)
			}
			return "—"
		}
		fmt.Printf("  %-16s gesture=%-9s dance=%-9s closeup=%-9s chat=%-9s none=%s\n",
			name, cell("gesture"), cell("dance"), cell("closeup"), cell("chat"), cell("none"))
	}
}
