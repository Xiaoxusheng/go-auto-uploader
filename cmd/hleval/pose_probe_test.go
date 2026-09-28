package main

import (
	"fmt"
	"math"
	"testing"
)

// ppDanceWin 构造舞窗序列：全身大幅位移（躯干+四肢都在动，踝可见）。
func ppDanceWin(sec int) ppWindow {
	frames := make([]ppFrame, 0, 8)
	for i := 0; i < 8; i++ {
		s := float64(sec + i)
		frames = append(frames, ppFrame{
			Sec: s,
			MSX: 300 + 8*math.Sin(s*2), MSY: 200 + 6*math.Cos(s*2),
			MHX: 300 + 10*math.Sin(s*2+1), MHY: 300 + 8*math.Cos(s*2+1),
			LWX: 250 + 40*math.Sin(s*3), LWY: 250 + 35*math.Cos(s*3), LWC: 0.9,
			RWX: 350 + 40*math.Sin(s*3+2), RWY: 250 + 35*math.Cos(s*3+2), RWC: 0.9,
			LAX: 270 + 25*math.Sin(s*2.5), LAY: 400 + 20*math.Cos(s*2.5), LAC: 0.9,
			RAX: 330 + 25*math.Sin(s*2.5+1), RAY: 400 + 20*math.Cos(s*2.5+1), RAC: 0.9,
		})
	}
	return ppWindow{Clip: "c", Sec: sec, Label: "dance", Frames: frames}
}

// ppGestureWin 构造手势聊天窗序列：躯干近静止、腕部中等活动、踝出画（半身构图）。
func ppGestureWin(sec int) ppWindow {
	frames := make([]ppFrame, 0, 8)
	for i := 0; i < 8; i++ {
		s := float64(sec + i)
		frames = append(frames, ppFrame{
			Sec: s,
			MSX: 300 + 1.5*math.Sin(s), MSY: 200 + 1.0*math.Cos(s),
			MHX: 300 + 1.0*math.Sin(s+1), MHY: 300 + 1.2*math.Cos(s+1),
			LWX: 260 + 25*math.Sin(s*4), LWY: 240 + 20*math.Cos(s*4), LWC: 0.9,
			RWX: 340 + 25*math.Sin(s*4+2), RWY: 240 + 20*math.Cos(s*4+2), RWC: 0.9,
			LAC: 0.05, RAC: 0.05, // 踝不可见
		})
	}
	return ppWindow{Clip: "c", Sec: sec, Label: "gesture", Frames: frames}
}

// TestPPFeaturesDirection 合成序列上验证特征方向：舞 torso_ratio 高、手势低；
// 手势 ankle_vis 低（≈0）舞高；手势 wrist_ankle_ratio 为 NaN（踝不可见）。
func TestPPFeaturesDirection(t *testing.T) {
	fd := ppFeatures(ppDanceWin(0))
	fg := ppFeatures(ppGestureWin(0))
	if fd["torso_ratio"] <= fg["torso_ratio"] {
		t.Fatalf("torso_ratio 方向错: dance=%v gesture=%v（应 dance>gesture）", fd["torso_ratio"], fg["torso_ratio"])
	}
	if math.Abs(fg["ankle_vis"]) > 1e-9 {
		t.Fatalf("gesture ankle_vis 应≈0，得 %v", fg["ankle_vis"])
	}
	if fd["ankle_vis"] < 0.99 {
		t.Fatalf("dance ankle_vis 应≈1，得 %v", fd["ankle_vis"])
	}
	if !math.IsNaN(fg["wrist_ankle_ratio"]) {
		t.Fatalf("gesture 踝不可见时 wrist_ankle_ratio 应 NaN，得 %v", fg["wrist_ankle_ratio"])
	}
	if math.IsNaN(fd["wrist_ankle_ratio"]) {
		t.Fatalf("dance wrist_ankle_ratio 不应 NaN")
	}
	if math.IsNaN(fd["dir_reversal"]) || math.IsNaN(fg["dir_reversal"]) {
		t.Fatalf("dir_reversal 出现意外 NaN: dance=%v gesture=%v", fd["dir_reversal"], fg["dir_reversal"])
	}
}

// TestPPFeaturesTooFewFrames 帧不足的窗全特征 NaN。
func TestPPFeaturesTooFewFrames(t *testing.T) {
	w := ppDanceWin(0)
	w.Frames = w.Frames[:3]
	f := ppFeatures(w)
	for _, k := range ppFeatureNames {
		if !math.IsNaN(f[k]) {
			t.Fatalf("%s 应 NaN，得 %v", k, f[k])
		}
	}
}

// TestPPAUCBasic AUC 排序正确性：完美分离=1，反向=0，并列≈0.5，NaN 剔除。
func TestPPAUCBasic(t *testing.T) {
	if a, _, _ := ppAUC([]float64{2, 3, 4}, []float64{0, 1}); math.Abs(a-1) > 1e-9 {
		t.Fatalf("完美分离 AUC 应 1，得 %v", a)
	}
	if a, _, _ := ppAUC([]float64{0, 1}, []float64{2, 3, 4}); math.Abs(a) > 1e-9 {
		t.Fatalf("反向 AUC 应 0，得 %v", a)
	}
	if a, _, _ := ppAUC([]float64{1, 1, 2}, []float64{1, 2, 2}); math.Abs(a-0.5) > 0.2 {
		t.Fatalf("混合 AUC 应近 0.5，得 %v", a)
	}
	if a, _, _ := ppAUC([]float64{1, math.NaN()}, []float64{0}); math.Abs(a-1) > 1e-9 {
		t.Fatalf("NaN 应剔除：AUC 应 1，得 %v", a)
	}
	if a, np, _ := ppAUC([]float64{math.NaN(), math.NaN()}, []float64{0}); !math.IsNaN(a) || np != 0 {
		t.Fatalf("全 NaN 应返回 NaN/0，得 %v nPos=%d", a, np)
	}
}

// TestPPSelectWindowsDeterminism 同种子同结果、跨种子大概率不同；gesture 全量保留。
func TestPPSelectWindowsDeterminism(t *testing.T) {
	gold := map[string]map[string]string{}
	for i := 0; i < 40; i++ {
		clip := fmt.Sprintf("%c_clip", 'a'+i%26) + fmt.Sprint(i/26)
		gold[clip] = map[string]string{
			"0":  "dance",
			"8":  "closeup",
			"16": "chat",
		}
	}
	gold["g1"] = map[string]string{"0": "gesture", "8": "gesture"}
	a := ppSelectWindows(gold, 30, 20, 20, 42)
	b := ppSelectWindows(gold, 30, 20, 20, 42)
	c := ppSelectWindows(gold, 30, 20, 20, 7)
	if len(a) != len(b) {
		t.Fatalf("同种子窗数不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同种子第 %d 窗不同: %+v vs %+v", i, a[i], b[i])
		}
	}
	same := len(a) == len(c)
	if same {
		for i := range a {
			if a[i] != c[i] {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatalf("不同种子结果完全相同，采样疑似未生效")
	}
	ng, nd := 0, 0
	for _, s := range a {
		switch s.Label {
		case "gesture":
			ng++
		case "dance":
			nd++
		}
	}
	if ng != 2 {
		t.Fatalf("gesture 应全量保留 2 窗，得 %d", ng)
	}
	if nd != 30 {
		t.Fatalf("dance 总量 40 > 采样上限 30 应恰取 30 窗，得 %d", nd)
	}
}

// TestPPVetoSimulation 否决规则只作用于门判舞窗且特征缺失不否决。
func TestPPVetoSimulation(t *testing.T) {
	rows := []ppRow{
		{Clip: "g1", Label: "gesture", GateDance: true, AnkleVis: ppPtr(0.0)},
		{Clip: "g2", Label: "gesture", GateDance: true, AnkleVis: nil}, // 特征缺失
		{Clip: "g3", Label: "gesture", GateDance: false, AnkleVis: ppPtr(0.0)},
		{Clip: "d1", Label: "dance", GateDance: true, AnkleVis: ppPtr(1.0)},
		{Clip: "d2", Label: "dance", GateDance: true, AnkleVis: ppPtr(0.1)},
	}
	byClass := map[string][]*ppRow{}
	for i := range rows {
		byClass[rows[i].Label] = append(byClass[rows[i].Label], &rows[i])
	}
	vs := ppVetoSimulation(rows, byClass, map[string]int{})
	if len(vs) == 0 {
		t.Fatal("应产出规则")
	}
	v := vs[0] // ankle_vis<0.25
	if v.GestureBefore != 2 || v.GestureAfter != 1 || v.GestureTotal != 3 {
		t.Fatalf("gesture 计数错: %+v", v)
	}
	if v.DanceGateDance != 2 || v.DanceKill != 1 {
		t.Fatalf("dance 计数错: %+v", v)
	}
}

// pp2Synth 构造构形窗：danceLike=手臂高举远离脸、肩线旋转、头部移动；
// gestureLike=手贴脸、肩线稳定、头部微动。
func pp2Synth(danceLike bool) ppWindow {
	frames := make([]ppFrame, 0, 8)
	cx, cy := 300.0, 300.0
	for i := 0; i < 8; i++ {
		s := float64(i)
		f := ppFrame{
			Sec: s,
			MSX: cx, MSY: cy - 40, MHX: cx, MHY: cy + 40,
			LSX: cx - 30, LSY: cy - 40, LSC: 0.9,
			RSX: cx + 30, RSY: cy - 40, RSC: 0.9,
			NX: cx, NY: cy - 90, NC: 0.9,
			LEX: cx - 8, LEY: cy - 95, LEC: 0.9,
			REX: cx + 8, REY: cy - 95, REC: 0.9,
			LARX: cx - 20, LARY: cy - 90, LARC: 0.9,
			RARX: cx + 20, RARY: cy - 90, RARC: 0.9,
		}
		if danceLike {
			rot := 0.5 * math.Sin(s) // 肩线大幅旋转
			f.RSX = cx - 30 + 60*math.Cos(rot)
			f.RSY = cy - 40 + 60*math.Sin(rot)
			f.LSX = cx - 30 - 20*math.Cos(rot)
			f.LSY = cy - 40 - 20*math.Sin(rot)
			f.LWX, f.LWY, f.LWC = cx-80, cy-140, 0.9 // 高举远离脸
			f.RWX, f.RWY, f.RWC = cx+80, cy-130, 0.9
			f.LELX, f.LELY, f.LELC = cx-60, cy-90, 0.9
			f.RELX, f.RELY, f.RELC = cx+60, cy-85, 0.9
			f.NX = cx + 15*math.Sin(s*2) // 头随身体动
			f.NY = cy - 90 + 8*math.Cos(s*2)
		} else {
			f.LWX, f.LWY, f.LWC = cx-5, cy-88, 0.9 // 手贴脸下方
			f.RWX, f.RWY, f.RWC = cx+6, cy-85, 0.9
			f.LELX, f.LELY, f.LELC = cx-25, cy-50, 0.9
			f.RELX, f.RELY, f.RELC = cx+25, cy-50, 0.9
			f.NX = cx + 2*math.Sin(s) // 头微动
		}
		frames = append(frames, f)
	}
	return ppWindow{Clip: "c", Sec: 0, Label: "x", Frames: frames}
}

func TestPP2FeaturesDirection(t *testing.T) {
	fd := pp2Features(pp2Synth(true))
	fg := pp2Features(pp2Synth(false))
	if math.IsNaN(fd.FaceTouch) || math.IsNaN(fg.FaceTouch) {
		t.Fatalf("face_touch NaN: dance=%v gesture=%v", fd.FaceTouch, fg.FaceTouch)
	}
	if !(fg.FaceTouch > fd.FaceTouch) {
		t.Fatalf("face_touch 方向错: gesture=%.2f dance=%.2f（应 gesture>dance）", fg.FaceTouch, fd.FaceTouch)
	}
	// 脸在肩线上方时手贴脸也高于肩线，wrist_high 在此合成几何下双方均为 1，不设方向断言
	if !(fd.WristSpanMean > fg.WristSpanMean) {
		t.Fatalf("wrist_span_mean 方向错: dance=%.2f gesture=%.2f（应 dance>gesture）", fd.WristSpanMean, fg.WristSpanMean)
	}
	if !(fd.ShoAngleStd > fg.ShoAngleStd) {
		t.Fatalf("sho_angle_std 方向错: dance=%.3f gesture=%.3f", fd.ShoAngleStd, fg.ShoAngleStd)
	}
	if !(fd.NoseSpeed > fg.NoseSpeed) {
		t.Fatalf("nose_speed 方向错: dance=%.3f gesture=%.3f", fd.NoseSpeed, fg.NoseSpeed)
	}
}
