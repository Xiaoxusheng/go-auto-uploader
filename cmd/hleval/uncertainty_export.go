// uncertainty-export 金标飞轮取样（§31 落地 + 飞轮第 4/5 批升级）。
//
// 三种模式（-mode）：
//   - band（第 1-3 批用法）：导出「门不确定带」窗 + 随机对照窗——生产门三阈值
//     只伺候一个切点，判别力短板恰好在切点附近（vis 0.45~0.80 / face 0.08~0.16）。
//   - verdict（第 4 批）：按「门∧头」最终系统判定取样，专标两类残余错误：
//     keep   = 门通过 ∧ 头判舞（生产会保留）→ 标注找聊天/手势类残余误报（头下一轮难负例）
//     reject = 门通过 ∧ 头拒绝（生产会压掉）→ 标注确认真舞不被头误杀（护 Recall）
//   - segment（第 5 批起）：段级整段标注。段标签需要整段窗覆盖，故：
//     ①枚举池内全部「门通过段」（生产三阈值 + 现值头打分，段=门通过窗的极大连续游程）；
//     ②优先级 A=含第 4 批 keep∧非舞难例的段（误报主攻）、B=含 reject∧舞难例的段
//     （护 Recall）、C=其余；A/B 全收，C 按主播轮转补到 -total；
//     ③只导出段内【未标注】窗（gold ∪ exclude2 已标窗跳过，段标签靠补齐覆盖）。
//     门三阈值读生产 config.json 现值（读不到回退 0.70/0.12/0.30 并告警）；
//     头打分复用 internal/pose 头评估器（与 8080 灰度同一 gate_head 产物）。
//
// 选择规则（确定性，seed 可复现）：
//   - 资格：池内片 ∧ 有每秒特征 ∧ 帧目录有效；band/verdict 另排除已金标片（整片），
//     segment 用窗级排除（金标片内未标窗仍可导出）
//   - band：带内窗 det_rate≥0.3 ∧ (0.45≤vis_mean≤0.80 ∨ 0.08≤face_mean≤0.16)，带外为对照
//   - verdict：门通过窗（det≥det_min ∧ vis≥vis_min ∧ face≤face_max）按头打分二分
//   - 每片上限：band 带内 -per-clip / 对照 4；verdict 每类 ⌈-per-clip/2⌉；segment 段整取
//   - 总量 -total：band 带内优先、对照约 1/3；verdict 两类各半；segment 约束 C 类补量
//
// 用法：
//
//	hleval uncertainty-export [-mode band|verdict|segment] [-config <json>] [-pose <json>]
//	                         [-gold <json>] [-exclude2 <json>] [-frames <dir>]
//	                         [-out <json>] [-head <json>] [-live-config <json>]
//	                         [-b4idx <json>] [-b4labels <json>]
//	                         [-per-clip N] [-total N] [-seed N]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"upload/internal/pose"
)

const (
	ueWin       = 8
	ueDetMin    = 0.3
	ueVisLo     = 0.45
	ueVisHi     = 0.80
	ueFaceLo    = 0.08
	ueFaceHi    = 0.16
	ueMinFrames = 20

	// ueHeadDanceProb 头窗口级判舞阈：与部署段级投票（headSegmentVote）内部判据一致。
	ueHeadDanceProb = 0.5
)

// ueWindow 一个候选窗。band 模式只有前 6 个字段；verdict/segment 模式附加门/头判定字段。
type ueWindow struct {
	Clip     string  `json:"clip"`
	Sec      int     `json:"sec"`
	DetRate  float64 `json:"det_rate"`
	VisMean  float64 `json:"vis_mean"`
	FaceMean float64 `json:"face_mean"`
	Band     bool    `json:"band"`

	GatePass bool    `json:"gate_pass,omitempty"`
	HeadProb float64 `json:"head_prob,omitempty"` // 头 dance 概率（3 位小数）
	Cls      string  `json:"cls,omitempty"`       // keep | reject
	Streamer string  `json:"streamer,omitempty"`
	SegID    int     `json:"seg_id,omitempty"` // segment 模式：所属段
	SegPrio  string  `json:"seg_prio,omitempty"`
}

// ueGate 生产门三阈值（verdict/segment 模式用）。
type ueGate struct {
	VisMin, FaceMax, DetMin float64
	Source                  string // live-config | fallback
}

// ueGatePass 生产门口径：det率<detmin → 非舞；否则检出秒 vis 均值≥visMin 且
// face 均值≤faceMax → 舞。与生产门 / autogold-sweep 同一谓词，不另写数学。
func ueGatePass(st pose.WindowStats, g ueGate) bool {
	return st.DetRate >= g.DetMin && st.VisMean >= g.VisMin && st.FaceMean <= g.FaceMax
}

// ueLoadLiveGate 从生产 config.json 读门现值；读不到回退 2026-09-27 定标点并告警。
func ueLoadLiveGate(path string) ueGate {
	g := ueGate{VisMin: 0.70, FaceMax: 0.12, DetMin: 0.30, Source: "fallback"}
	var top struct {
		Builtin struct {
			HighlightPoseGate *struct {
				VisMin  float64 `json:"vis_min"`
				FaceMax float64 `json:"face_max"`
				DetMin  float64 `json:"det_min"`
			} `json:"highlight_pose_gate"`
		} `json:"builtin"`
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &top) == nil && top.Builtin.HighlightPoseGate != nil {
		p := top.Builtin.HighlightPoseGate
		g.VisMin, g.FaceMax, g.DetMin, g.Source = p.VisMin, p.FaceMax, p.DetMin, "live-config"
	} else {
		fmt.Fprintf(os.Stderr, "警告: 读生产门配置失败 %s，回退 0.70/0.12/0.30\n", path)
	}
	return g
}

// ueStreamerRe 与 api/http pose_training.go 的 poseClipRe 同口径（hleval 不 import
// api/http，正则两处同步维护——改一处必须同步另一处）。
var ueStreamerRe = regexp.MustCompile(`^(.+)_(\d{4}-\d{2}-\d{2})_(\d{2}-\d{2}-\d{2})_\d+$`)

func ueStreamerName(clip string) string {
	if m := ueStreamerRe.FindStringSubmatch(clip); m != nil {
		return m[1]
	}
	return ""
}

// ueFilterStreamers -streamers 白名单过滤（定向补采）：空 spec=原样返回；
// 逗号分隔主播名，与 ueStreamerName 同口径。冻结 v3 主播即使写进白名单也被剔除
// （红线优先于定向）。
func ueFilterStreamers(cfg []clipConfigEntry, spec string) []clipConfigEntry {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return cfg
	}
	allow := map[string]bool{}
	for _, s := range strings.Split(spec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			allow[s] = true
		}
	}
	out := cfg[:0]
	for _, e := range cfg {
		if allow[ueStreamerName(e.Clip)] && !ueFrozenStreamers[ueStreamerName(e.Clip)] {
			out = append(out, e)
		}
	}
	return out
}

// ueFrozenStreamers 冻结集 v3 验收主播（主播级双 OOS 红线）：其任何窗不得进入
// 金标/训练/飞轮采样，否则验收集失去独立性（2026-09-28 batch6 污染事件加固，
// 见 highlight-progress.md §43.4/§44.4）。新建冻结集时同步维护此表。
var ueFrozenStreamers = map[string]bool{"倦": true, "小皮": true, "颜兮": true}

// ueFrozenExcluded 采样资格红线：冻结 v3 主播的片一律不参与飞轮取样。
func ueFrozenExcluded(clip string) bool {
	return ueFrozenStreamers[ueStreamerName(clip)]
}

// ueClassifyVerdict 一窗的最终系统语义：门通过时按头打分二分（keep=生产会保留、
// reject=生产会压掉），门不通过返回 ok=false。逐秒特征须带全部 5 维（头用 ext/aspect）。
func ueClassifyVerdict(clip string, sec int, win []pose.FrameFeatures, g ueGate, m *pose.HeadModel) (ueWindow, bool) {
	st := pose.AggregateWindow(win, 0, 0, 0)
	if !ueGatePass(st, g) {
		return ueWindow{}, false
	}
	prob := m.HeadProb(pose.HeadFeatures(win))
	w := ueWindow{
		Clip: clip, Sec: sec,
		DetRate:  st.DetRate,
		VisMean:  st.VisMean,
		FaceMean: st.FaceMean,
		Band: st.DetRate >= ueDetMin &&
			((st.VisMean >= ueVisLo && st.VisMean <= ueVisHi) || (st.FaceMean >= ueFaceLo && st.FaceMean <= ueFaceHi)),
		GatePass: true,
		HeadProb: math.Round(prob*1000) / 1000,
		Streamer: ueStreamerName(clip),
	}
	if prob >= ueHeadDanceProb {
		w.Cls = "keep"
	} else {
		w.Cls = "reject"
	}
	return w, true
}

// ueFairTake 主播间轮转均衡取样：每轮给每个还有余量的主播取一窗，直到 budget
// 或全耗尽（先取尽的主播退出轮转）。确定性：主播名排序；rng 只用于主播内洗牌。
func ueFairTake(byStreamer map[string][]ueWindow, budget int, rng *rand.Rand) []ueWindow {
	names := make([]string, 0, len(byStreamer))
	for n := range byStreamer {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rng.Shuffle(len(byStreamer[n]), func(i, j int) { byStreamer[n][i], byStreamer[n][j] = byStreamer[n][j], byStreamer[n][i] })
	}
	idx := map[string]int{}
	out := make([]ueWindow, 0, budget)
	for len(out) < budget {
		progress := false
		for _, n := range names {
			if len(out) >= budget {
				break
			}
			if idx[n] < len(byStreamer[n]) {
				out = append(out, byStreamer[n][idx[n]])
				idx[n]++
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return out
}

// ueGroupByStreamer 按主播分组（解析失败的归入同一桶，仍参与均衡）。
func ueGroupByStreamer(wins []ueWindow) map[string][]ueWindow {
	m := map[string][]ueWindow{}
	for _, w := range wins {
		n := w.Streamer
		if n == "" {
			n = "(未解析主播)"
		}
		m[n] = append(m[n], w)
	}
	return m
}

// ueStreamerStat 输出 JSON 里的主播分布行。
type ueStreamerStat struct {
	Streamer string `json:"streamer"`
	Keep     int    `json:"keep"`
	Reject   int    `json:"reject"`
	Total    int    `json:"total"`
}

// ueSegment segment 模式的一个门通过段。
type ueSegment struct {
	ID        int     `json:"id"`
	Clip      string  `json:"clip"`
	Streamer  string  `json:"streamer"`
	StartSec  int     `json:"start_sec"`
	EndSec    int     `json:"end_sec"` // 末窗起始秒 + 8（近似，尾短窗从宽）
	NWin      int     `json:"n_win"`
	FracDance float64 `json:"frac_dance"` // 段内头判舞窗占比（生产段级投票同口径）
	Priority  string  `json:"priority"`   // A=含keep非舞难例 B=含reject真舞难例 C=其余
	NKeepFP   int     `json:"n_keep_fp"`
	NRejDan   int     `json:"n_rej_dan"`
	NMissing  int     `json:"n_missing"` // 未标注窗数（本次导出数）

	Missing []ueWindow `json:"-"` // 待标注窗（导出前展开进 windows）
}

func cmdUncertaintyExport(args []string) {
	fs := flag.NewFlagSet("uncertainty-export", flag.ExitOnError)
	mode := fs.String("mode", "band", "取样模式：band=门不确定带+对照（第 1-3 批）| verdict=门∧头最终判定两类（第 4 批）| segment=段级整段标注（第 5 批）")
	cfgPath := fs.String("config", "D:/upload/_diag/train/_pose_pilot/clips_config.json", "池配置 JSON")
	posePath := fs.String("pose", "D:/upload/_diag/train/pose_features_go.json", "每秒姿态特征 JSON")
	goldPath := fs.String("gold", "D:/upload/_diag/train/_pose_pilot/gold_review.json", "金标（排除集 1）")
	exclude2 := fs.String("exclude2", "D:/upload/_diag/train/audio_probe/audio_probe_gold.json", "追加排除集（如盲标待并入集）")
	framesRoot := fs.String("frames", "D:/upload/_diag/train/_pose_pilot/frames", "帧根目录（有效性检查）")
	outPath := fs.String("out", "D:/upload/_diag/train/audio_probe/uncertainty_batch.json", "导出 JSON")
	headPath := fs.String("head", "D:/upload/_diag/train/gate_head/gate_head_v2_trees.json", "学习型头模型 JSON（verdict/segment 模式）")
	liveCfgPath := fs.String("live-config", "D:/upload/config.json", "生产 config.json（verdict/segment 模式读门三阈值现值）")
	b4Idx := fs.String("b4idx", "D:/upload/_diag/train/audio_probe/usheets4/index.json", "第 4 批 sheet index（segment 模式定位难例段）")
	b4Labels := fs.String("b4labels", "D:/upload/_diag/train/audio_probe/labels4.json", "第 4 批窗标签（segment 模式）")
	perClip := fs.Int("per-clip", 8, "每片窗上限（band=带内窗上限；verdict=每类上限取其半、向上取整；segment 不适用）")
	total := fs.Int("total", 360, "导出总窗上限（segment 模式约束 C 类补量，A/B 段不受限）")
	seed := fs.Int64("seed", 42, "随机种子")
	streamers := fs.String("streamers", "", "逗号分隔主播名白名单（定向补采，如新手势型主播），空=全池")
	fs.Parse(args)

	switch *mode {
	case "band", "verdict", "segment":
	default:
		fmt.Fprintf(os.Stderr, "未知 mode %q（可选 band|verdict|segment）\n", *mode)
		os.Exit(1)
	}

	cfg := []clipConfigEntry{}
	if b, err := os.ReadFile(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "读池配置失败: %v\n", err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "解析池配置失败: %v\n", err)
		os.Exit(1)
	}
	cfg = ueFilterStreamers(cfg, *streamers)
	excludeClip := map[string]bool{}
	excludeWins := map[string]map[int]bool{} // segment 模式窗级排除：已标注窗
	for _, p := range []string{*goldPath, *exclude2} {
		g := map[string]map[string]string{}
		if b, err := os.ReadFile(p); err == nil {
			if json.Unmarshal(b, &g) == nil {
				for c, wins := range g {
					excludeClip[c] = true
					if excludeWins[c] == nil {
						excludeWins[c] = map[int]bool{}
					}
					for k := range wins {
						if s, err := strconv.Atoi(k); err == nil {
							excludeWins[c][s] = true
						}
					}
				}
			}
		}
	}
	poseFeats := map[string]agClipFeats{}
	if b, err := os.ReadFile(*posePath); err != nil {
		fmt.Fprintf(os.Stderr, "读姿态特征失败: %v\n", err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &poseFeats); err != nil {
		fmt.Fprintf(os.Stderr, "解析姿态特征失败: %v\n", err)
		os.Exit(1)
	}

	rng := rand.New(rand.NewSource(*seed))
	// allClips：有姿态特征+帧目录的池片；clips：另排除已金标整片（band/verdict 用）。
	// 冻结 v3 主播片在源头剔除（三种模式共用此入口）。
	allClips := make([]string, 0, len(cfg))
	frozen := 0
	for _, e := range cfg {
		c := e.Clip
		if _, ok := poseFeats[c]; !ok {
			continue
		}
		if ueFrozenExcluded(c) {
			frozen++
			continue
		}
		if frameFiles, _ := filepath.Glob(filepath.Join(*framesRoot, c, "f_*.jpg")); len(frameFiles) < ueMinFrames {
			continue
		}
		allClips = append(allClips, c)
	}
	sort.Strings(allClips)
	clips := make([]string, 0, len(allClips))
	for _, c := range allClips {
		if !excludeClip[c] {
			clips = append(clips, c)
		}
	}
	fmt.Printf("资格片: %d（池 %d，已金标整片 %d，冻结v3主播片 %d）\n", len(clips), len(cfg), len(excludeClip), frozen)

	var band, ctrl []ueWindow
	var keep, reject []ueWindow
	var all []ueWindow
	var gate ueGate
	var head *pose.HeadModel
	var segList []ueSegment
	if *mode == "band" {
		for _, c := range clips {
			feats := poseFeats[c].Feats
			var cBand, cCtrl []ueWindow
			for s := 0; s+ueWin <= len(feats); s += ueWin {
				win := make([]pose.FrameFeatures, ueWin)
				for i, f := range feats[s : s+ueWin] {
					win[i] = pose.FrameFeatures{Detected: f[4] == 1, VisRatio: f[0], FaceFrac: f[1]}
				}
				st := pose.AggregateWindow(win, 0, 0, 0)
				w := ueWindow{Clip: c, Sec: s, DetRate: st.DetRate, VisMean: st.VisMean, FaceMean: st.FaceMean}
				inBand := st.DetRate >= ueDetMin &&
					((st.VisMean >= ueVisLo && st.VisMean <= ueVisHi) || (st.FaceMean >= ueFaceLo && st.FaceMean <= ueFaceHi))
				w.Band = inBand
				if inBand {
					cBand = append(cBand, w)
				} else {
					cCtrl = append(cCtrl, w)
				}
			}
			// 片内随机取样上限（避免同一片刷屏；全局单 rng，片序确定故可复现）
			rng.Shuffle(len(cBand), func(i, j int) { cBand[i], cBand[j] = cBand[j], cBand[i] })
			rng.Shuffle(len(cCtrl), func(i, j int) { cCtrl[i], cCtrl[j] = cCtrl[j], cCtrl[i] })
			if len(cBand) > *perClip {
				cBand = cBand[:*perClip]
			}
			if len(cCtrl) > 4 {
				cCtrl = cCtrl[:4]
			}
			band = append(band, cBand...)
			ctrl = append(ctrl, cCtrl...)
		}
		fmt.Printf("带内窗 %d / 对照窗 %d（取样前）\n", len(band), len(ctrl))

		// 总量控制：带内优先，对照占约 1/3
		nCtrl := *total / 3
		if len(ctrl) > nCtrl {
			ctrl = ctrl[:nCtrl]
		}
		if len(band)+len(ctrl) > *total {
			band = band[:*total-len(ctrl)]
		}
		all = append(band, ctrl...)
	} else if *mode == "verdict" {
		gate = ueLoadLiveGate(*liveCfgPath)
		var err error
		head, err = pose.LoadHeadModel(*headPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读头模型失败 %s: %v\n", *headPath, err)
			os.Exit(1)
		}
		perClass := (*perClip + 1) / 2
		for _, c := range clips {
			feats := poseFeats[c].Feats
			var cKeep, cReject []ueWindow
			for s := 0; s+ueWin <= len(feats); s += ueWin {
				win := make([]pose.FrameFeatures, ueWin)
				for i, f := range feats[s : s+ueWin] {
					win[i] = pose.FrameFeatures{Detected: f[4] == 1, VisRatio: f[0], FaceFrac: f[1], ExtH: f[2], Aspect: f[3]}
				}
				if w, ok := ueClassifyVerdict(c, s, win, gate, head); ok {
					if w.Cls == "keep" {
						cKeep = append(cKeep, w)
					} else {
						cReject = append(cReject, w)
					}
				}
			}
			// 片内随机取样上限（同类内洗牌；全局单 rng，片序确定故可复现）
			rng.Shuffle(len(cKeep), func(i, j int) { cKeep[i], cKeep[j] = cKeep[j], cKeep[i] })
			rng.Shuffle(len(cReject), func(i, j int) { cReject[i], cReject[j] = cReject[j], cReject[i] })
			if len(cKeep) > perClass {
				cKeep = cKeep[:perClass]
			}
			if len(cReject) > perClass {
				cReject = cReject[:perClass]
			}
			keep = append(keep, cKeep...)
			reject = append(reject, cReject...)
		}
		fmt.Printf("门通过窗（取样前）：keep %d / reject %d（门 %s：vis %.2f / face %.2f / det %.2f）\n",
			len(keep), len(reject), gate.Source, gate.VisMin, gate.FaceMax, gate.DetMin)

		// 主播均衡：两类各半预算、类内轮转；一类稀缺时余量自动让给另一类
		gKeep, gReject := ueGroupByStreamer(keep), ueGroupByStreamer(reject)
		kb, rb := *total/2, *total-*total/2
		keep, reject = ueFairTake(gKeep, kb, rng), ueFairTake(gReject, rb, rng)
		if len(keep)+len(reject) < *total {
			if len(keep) < kb {
				reject = ueFairTake(gReject, rb+(kb-len(keep)), rng)
			} else if len(reject) < rb {
				keep = ueFairTake(gKeep, kb+(rb-len(reject)), rng)
			}
		}
		all = append(keep, reject...)
	} else {
		// segment：门通过段枚举 + 难例段优先 + 窗级排除（只导未标注窗）
		gate = ueLoadLiveGate(*liveCfgPath)
		var err error
		head, err = pose.LoadHeadModel(*headPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读头模型失败 %s: %v\n", *headPath, err)
			os.Exit(1)
		}
		// 第 4 批难例窗（clip,sec → kind）：keep∧非舞=误报锚点，reject∧舞=护 Recall 锚点
		b4 := map[string]map[int]string{}
		var idxFiles []struct {
			Windows []struct {
				I    int    `json:"i"`
				Clip string `json:"clip"`
				Sec  int    `json:"sec"`
				Cls  string `json:"cls"`
			} `json:"windows"`
		}
		if b, err := os.ReadFile(*b4Idx); err == nil {
			if err := json.Unmarshal(b, &idxFiles); err != nil {
				fmt.Fprintf(os.Stderr, "解析第 4 批 index 失败: %v\n", err)
				os.Exit(1)
			}
			lab := map[string]string{}
			if lb, err := os.ReadFile(*b4Labels); err == nil {
				_ = json.Unmarshal(lb, &lab)
			}
			for _, sh := range idxFiles {
				for _, w := range sh.Windows {
					l, ok := lab[strconv.Itoa(w.I)]
					if !ok {
						continue
					}
					isDance := l == "dance"
					kind := ""
					if w.Cls == "keep" && !isDance {
						kind = "keep_nondance"
					} else if w.Cls == "reject" && isDance {
						kind = "reject_dance"
					}
					if kind != "" {
						if b4[w.Clip] == nil {
							b4[w.Clip] = map[int]string{}
						}
						b4[w.Clip][w.Sec] = kind
					}
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "警告: 读第 4 批 index 失败 %s（A/B 优先级失效）\n", *b4Idx)
		}
		nHard := 0
		for _, m := range b4 {
			nHard += len(m)
		}
		fmt.Printf("第 4 批难例窗: %d（应 144）\n", nHard)

		var segs []ueSegment
		for _, c := range allClips {
			feats := poseFeats[c].Feats
			labeled := excludeWins[c]
			type wv struct {
				s    int
				st   pose.WindowStats
				prob float64
				pass bool
			}
			ws := make([]wv, 0, len(feats)/ueWin+1)
			for s := 0; s < len(feats); s += ueWin {
				win := make([]pose.FrameFeatures, 0, ueWin)
				hi := s + ueWin
				if hi > len(feats) {
					hi = len(feats) // 尾窗不足 8 秒按现存帧（与重定标口径一致），其余窗恒取 8 帧
				}
				for _, f := range feats[s:hi] {
					win = append(win, pose.FrameFeatures{Detected: f[4] == 1, VisRatio: f[0], FaceFrac: f[1], ExtH: f[2], Aspect: f[3]})
				}
				st := pose.AggregateWindow(win, 0, 0, 0)
				gp := ueGatePass(st, gate)
				prob := 0.0
				if gp {
					prob = head.HeadProb(pose.HeadFeatures(win))
				}
				ws = append(ws, wv{s, st, prob, gp})
			}
			// 门通过窗的极大连续游程 = 候选段
			for i := 0; i < len(ws); {
				if !ws[i].pass {
					i++
					continue
				}
				j := i
				for j+1 < len(ws) && ws[j+1].pass {
					j++
				}
				seg := ueSegment{Clip: c, Streamer: ueStreamerName(c), StartSec: ws[i].s, EndSec: ws[j].s + ueWin, NWin: j - i + 1}
				dance := 0
				for k := i; k <= j; k++ {
					if ws[k].prob >= ueHeadDanceProb {
						dance++
					}
					switch b4[c][ws[k].s] {
					case "keep_nondance":
						seg.NKeepFP++
					case "reject_dance":
						seg.NRejDan++
					}
					if !labeled[ws[k].s] {
						w := ueWindow{Clip: c, Sec: ws[k].s, DetRate: ws[k].st.DetRate, VisMean: ws[k].st.VisMean,
							FaceMean: ws[k].st.FaceMean, GatePass: true, HeadProb: math.Round(ws[k].prob*1000) / 1000,
							Streamer: seg.Streamer}
						if ws[k].prob >= ueHeadDanceProb {
							w.Cls = "keep"
						} else {
							w.Cls = "reject"
						}
						seg.Missing = append(seg.Missing, w)
					}
				}
				seg.FracDance = float64(dance) / float64(seg.NWin)
				seg.Priority = "C"
				if seg.NKeepFP > 0 {
					seg.Priority = "A"
				} else if seg.NRejDan > 0 {
					seg.Priority = "B"
				}
				segs = append(segs, seg)
				i = j + 1
			}
		}
		nA, nB, nC, missAB := 0, 0, 0, 0
		for _, sg := range segs {
			switch sg.Priority {
			case "A":
				nA++
				missAB += len(sg.Missing)
			case "B":
				nB++
				missAB += len(sg.Missing)
			default:
				nC++
			}
		}
		fmt.Printf("候选段 %d：A(含keep非舞) %d / B(含reject真舞) %d / C其他 %d；A+B 待标窗 %d\n", len(segs), nA, nB, nC, missAB)

		// 选择：A/B 全收（≥1 未标窗）；C 按主播轮转整段补到 -total
		var chosen []ueSegment
		abMiss := 0
		for i := range segs {
			if segs[i].Priority != "C" && len(segs[i].Missing) > 0 {
				chosen = append(chosen, segs[i])
				abMiss += len(segs[i].Missing)
			}
		}
		var cSegs []ueSegment
		for i := range segs {
			if segs[i].Priority == "C" && len(segs[i].Missing) > 0 {
				cSegs = append(cSegs, segs[i])
			}
		}
		sort.Slice(cSegs, func(a, b int) bool {
			if cSegs[a].Clip != cSegs[b].Clip {
				return cSegs[a].Clip < cSegs[b].Clip
			}
			return cSegs[a].StartSec < cSegs[b].StartSec
		})
		byStreamer := map[string][]ueSegment{}
		for _, sg := range cSegs {
			n := sg.Streamer
			if n == "" {
				n = "(未解析主播)"
			}
			byStreamer[n] = append(byStreamer[n], sg)
		}
		names := make([]string, 0, len(byStreamer))
		for n := range byStreamer {
			names = append(names, n)
		}
		sort.Strings(names)
		budget := *total - abMiss
		pos := map[string]int{}
		for budget > 0 {
			progress := false
			for _, n := range names {
				if budget <= 0 {
					break
				}
				if pos[n] < len(byStreamer[n]) {
					sg := byStreamer[n][pos[n]]
					pos[n]++
					chosen = append(chosen, sg)
					budget -= len(sg.Missing)
					progress = true
				}
			}
			if !progress {
				break
			}
		}
		// 段 ID 与窗扁平化
		sort.Slice(chosen, func(a, b int) bool {
			if chosen[a].Clip != chosen[b].Clip {
				return chosen[a].Clip < chosen[b].Clip
			}
			return chosen[a].StartSec < chosen[b].StartSec
		})
		for k := range chosen {
			chosen[k].ID = k
			chosen[k].NMissing = len(chosen[k].Missing)
			segList = append(segList, chosen[k])
			for _, w := range chosen[k].Missing {
				w.SegID = k
				w.SegPrio = chosen[k].Priority
				all = append(all, w)
			}
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Clip != all[j].Clip {
				return all[i].Clip < all[j].Clip
			}
			return all[i].Sec < all[j].Sec
		})
		nSegA, nSegB, nSegC := 0, 0, 0
		for _, sg := range segList {
			switch sg.Priority {
			case "A":
				nSegA++
			case "B":
				nSegB++
			default:
				nSegC++
			}
		}
		fmt.Printf("选中段 %d（A %d / B %d / C %d），待标窗 %d\n", len(segList), nSegA, nSegB, nSegC, len(all))
	}

	if *mode != "segment" {
		sort.Slice(all, func(i, j int) bool {
			if all[i].Clip != all[j].Clip {
				return all[i].Clip < all[j].Clip
			}
			return all[i].Sec < all[j].Sec
		})
	}

	out := map[string]interface{}{
		"generated_at": time.Now().Format("2006/1/2 15:04:05"),
		"seed":         *seed,
		"mode":         *mode,
		"windows":      all,
	}
	switch *mode {
	case "band":
		out["band_total"] = len(band)
		out["ctrl_total"] = len(ctrl)
	case "verdict":
		stats := map[string]*ueStreamerStat{}
		for _, w := range all {
			n := w.Streamer
			if n == "" {
				n = "(未解析主播)"
			}
			if stats[n] == nil {
				stats[n] = &ueStreamerStat{Streamer: n}
			}
			if w.Cls == "keep" {
				stats[n].Keep++
			} else {
				stats[n].Reject++
			}
			stats[n].Total++
		}
		names := make([]string, 0, len(stats))
		for n := range stats {
			names = append(names, n)
		}
		sort.Strings(names)
		statList := make([]*ueStreamerStat, 0, len(names))
		for _, n := range names {
			statList = append(statList, stats[n])
			fmt.Printf("  %s: keep %d / reject %d\n", n, stats[n].Keep, stats[n].Reject)
		}
		out["gate"] = map[string]interface{}{
			"vis_min": gate.VisMin, "face_max": gate.FaceMax, "det_min": gate.DetMin, "source": gate.Source,
		}
		out["head"] = map[string]interface{}{"version": head.Version, "train_set": head.TrainSet, "window_dance_prob": ueHeadDanceProb}
		out["keep_total"] = len(keep)
		out["reject_total"] = len(reject)
		out["streamers"] = statList
	default: // segment
		out["gate"] = map[string]interface{}{
			"vis_min": gate.VisMin, "face_max": gate.FaceMax, "det_min": gate.DetMin, "source": gate.Source,
		}
		out["head"] = map[string]interface{}{"version": head.Version, "train_set": head.TrainSet, "window_dance_prob": ueHeadDanceProb}
		out["segments"] = segList
		out["seg_total"] = len(segList)
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化失败: %v\n", err)
		os.Exit(1)
	}
	tmp := *outPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写失败: %v\n", err)
		os.Exit(1)
	}
	_ = os.Rename(tmp, *outPath)
	switch *mode {
	case "band":
		fmt.Printf("uncertainty-export 完成 → %s（带内 %d + 对照 %d = %d 窗，%d 片）\n",
			*outPath, len(band), len(ctrl), len(all), len(clips))
	case "verdict":
		fmt.Printf("uncertainty-export 完成 → %s（verdict：keep %d + reject %d = %d 窗，%d 片）\n",
			*outPath, len(keep), len(reject), len(all), len(clips))
	default:
		fmt.Printf("uncertainty-export 完成 → %s（segment：%d 段 / %d 待标窗）\n",
			*outPath, len(segList), len(all))
	}
}
