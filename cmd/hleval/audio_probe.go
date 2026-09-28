// audio-probe 音频通道探针：语音/音乐判别特征能否分离 dance vs gesture
// （gesture=手势聊天/轻晃，开封#5 盲区；用户拍板走音频方向，见
// _diag/train/audio_probe/ 与 docs/highlight-spatial-de.md §27）。
//
// 口径：
//   - 金标窗（audio_probe_gold.json，AI 盲标新片，schema 同 gold_review）× 源片
//     （audio_probe/sources/ 保护拷贝）ffmpeg 抽 16kHz 单声道 PCM → 自带 radix-2 FFT
//     零新依赖，纯 Go 无 CGO
//   - 假设覆盖「两边都有 BGM」情形：判别信号可能是语音主导度（说话窗人声明显、
//     音节调制 2-8Hz 强、有停顿）而非音乐存在性（舞/手势聊天可能都在放歌）
//   - 候选特征：beat_ac 节拍自相关强度 / bass_ratio 低频占比 / voice_mod 语音调制
//     主导度 / pause_count 语音停顿数 / flatness 谱平坦度 / dyn 包络起伏
//   - 评估：逐特征 AUC（正类=dance）+ 「现有门 ∧ 音频否决」模拟——gesture 判舞率
//     下降量 vs 舞误杀率是决策数字（与 pose-probe 同一决策口径）
//
// 用法：
//
//	hleval audio-probe [-gold <json>] [-sources <dir>] [-pose <json>] [-cache <json>]
//	                  [-out <json>] [-ffmpeg <path>] [-limit N]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"upload/internal/pose"
)

const (
	abSR        = 16000 // 重采样率（单声道）
	abWinSec    = 8
	abFrameSize = 1024 // STFT 帧长
	abHop       = 512  // STFT 帧移（env 采样率 = 16000/512 = 31.25Hz）
)

// abFeats 一窗音频特征（NaN=不可判）。
type abFeats struct {
	BeatAC    float64 `json:"beat_ac"`
	BassRatio float64 `json:"bass_ratio"`
	VoiceMod  float64 `json:"voice_mod"`
	PauseCnt  float64 `json:"pause_count"`
	Flatness  float64 `json:"flatness"`
	Dyn       float64 `json:"dyn"`
}

func (f abFeats) get(name string) float64 {
	switch name {
	case "beat_ac":
		return f.BeatAC
	case "bass_ratio":
		return f.BassRatio
	case "voice_mod":
		return f.VoiceMod
	case "pause_count":
		return f.PauseCnt
	case "flatness":
		return f.Flatness
	case "dyn":
		return f.Dyn
	}
	return math.NaN()
}

var abFeatureNames = []string{"beat_ac", "bass_ratio", "voice_mod", "pause_count", "flatness", "dyn"}

// abRow 一窗读数（特征 + 门统计），随结果落盘。
type abRow struct {
	Clip      string   `json:"clip"`
	Sec       int      `json:"sec"`
	Label     string   `json:"label"`
	DetRate   float64  `json:"det_rate"`
	VisMean   float64  `json:"vis_mean"`
	FaceMean  float64  `json:"face_mean"`
	GateDance bool     `json:"gate_dance"`
	BeatAC    *float64 `json:"beat_ac"`
	BassRatio *float64 `json:"bass_ratio"`
	VoiceMod  *float64 `json:"voice_mod"`
	PauseCnt  *float64 `json:"pause_count"`
	Flatness  *float64 `json:"flatness"`
	Dyn       *float64 `json:"dyn"`
}

func (r abRow) featPtr(name string) *float64 {
	switch name {
	case "beat_ac":
		return r.BeatAC
	case "bass_ratio":
		return r.BassRatio
	case "voice_mod":
		return r.VoiceMod
	case "pause_count":
		return r.PauseCnt
	case "flatness":
		return r.Flatness
	case "dyn":
		return r.Dyn
	}
	return nil
}

type abVetoOut struct {
	Rule          string `json:"rule"`
	GestureBefore int    `json:"gesture_before"`
	GestureAfter  int    `json:"gesture_after"`
	GestureTotal  int    `json:"gesture_total"`
	DanceGate     int    `json:"dance_gate_dance"`
	DanceKill     int    `json:"dance_kill"`
}

type abResult struct {
	GeneratedAt string           `json:"generated_at"`
	Windows     int              `json:"windows"`
	Features    map[string]*abFE `json:"features"`
	Veto        []*abVetoOut     `json:"veto_simulation"`
	Rows        []abRow          `json:"rows"`
}

type abFE struct {
	AUC       map[string]*float64 `json:"auc"`
	ClassMean map[string]*float64 `json:"class_mean"`
	Coverage  map[string]int      `json:"coverage"`
}

func cmdAudioProbe(args []string) {
	fs := flag.NewFlagSet("audio-probe", flag.ExitOnError)
	goldPath := fs.String("gold", "D:/upload/_diag/train/audio_probe/audio_probe_gold.json", "音频探针金标窗级标签 JSON")
	srcDir := fs.String("sources", "D:/upload/_diag/train/audio_probe/sources", "受保护源片目录")
	posePath := fs.String("pose", "D:/upload/_diag/train/pose_features_go.json", "每秒姿态特征 JSON（门统计/veto 用）")
	cachePath := fs.String("cache", "D:/upload/_diag/train/audio_probe/feat_cache.json", "特征缓存 JSON（增量续跑）")
	outPath := fs.String("out", "D:/upload/_diag/train/audio_probe_result.json", "结果 JSON")
	ffmpegBin := fs.String("ffmpeg", "D:/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe", "ffmpeg 路径")
	limit := fs.Int("limit", 0, "本次最多计算窗数（0=不限）")
	fs.Parse(args)

	gold := map[string]map[string]string{}
	if b, err := os.ReadFile(*goldPath); err != nil {
		fmt.Fprintf(os.Stderr, "读金标失败 %s: %v（先跑 Phase 1 盲标产出该文件）\n", *goldPath, err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &gold); err != nil {
		fmt.Fprintf(os.Stderr, "解析金标失败: %v\n", err)
		os.Exit(1)
	}
	cache := map[string]abFeats{}
	if b, err := os.ReadFile(*cachePath); err == nil {
		_ = json.Unmarshal(b, &cache)
	}

	// 枚举窗（clip 字典序、秒升序），增量跳过已缓存
	type win struct {
		clip  string
		sec   int
		label string
	}
	wins := []win{}
	clips := make([]string, 0, len(gold))
	for c := range gold {
		clips = append(clips, c)
	}
	sort.Strings(clips)
	for _, c := range clips {
		secs := []int{}
		for k := range gold[c] {
			if s, err := strconv.Atoi(k); err == nil {
				secs = append(secs, s)
			}
		}
		sort.Ints(secs)
		for _, s := range secs {
			wins = append(wins, win{c, s, gold[c][strconv.Itoa(s)]})
		}
	}
	todo := make([]win, 0, len(wins)) // 勿用 wins[:0]：与遍历中的 wins 共享底层数组会自我污染
	for _, w := range wins {
		if _, ok := cache[abKey(w.clip, w.sec)]; !ok {
			todo = append(todo, w)
		}
	}
	fmt.Printf("金标窗: %d，缓存命中: %d，待算: %d\n", len(wins), len(wins)-len(todo), len(todo))

	n := len(todo)
	if *limit > 0 && *limit < n {
		n = *limit
	}
	for i, w := range todo[:n] {
		src := filepath.Join(*srcDir, w.clip+".ts")
		pcm, err := abExtractPCM(*ffmpegBin, src, w.sec, abWinSec)
		if err != nil {
			fmt.Printf("  [%d/%d] %s@%ds 音频抽取失败: %v\n", i+1, n, w.clip, w.sec, err)
			continue
		}
		cache[abKey(w.clip, w.sec)] = abFeatures(pcm, abSR)
		fmt.Printf("  [%d/%d] %s@%ds %s\n", i+1, n, trunc(w.clip, 36), w.sec, w.label)
		if (i+1)%20 == 0 || i+1 == n {
			abSaveCache(*cachePath, cache)
		}
	}
	abSaveCache(*cachePath, cache)

	// 组装行 + 门统计
	poseFeats := map[string]agClipFeats{}
	if b, err := os.ReadFile(*posePath); err == nil {
		_ = json.Unmarshal(b, &poseFeats)
	}
	rows := make([]abRow, 0, len(wins))
	for _, w := range wins {
		f, ok := cache[abKey(w.clip, w.sec)]
		if !ok {
			continue
		}
		row := abRow{Clip: w.clip, Sec: w.sec, Label: w.label}
		if cf, ok := poseFeats[w.clip]; ok && w.sec < len(cf.Feats) {
			e := w.sec + abWinSec
			if e > len(cf.Feats) {
				e = len(cf.Feats)
			}
			fw := make([]pose.FrameFeatures, e-w.sec)
			for i, x := range cf.Feats[w.sec:e] {
				fw[i] = pose.FrameFeatures{Detected: x[4] == 1, VisRatio: x[0], FaceFrac: x[1]}
			}
			st := pose.AggregateWindow(fw, 0, 0, 0)
			row.DetRate, row.VisMean, row.FaceMean = st.DetRate, st.VisMean, st.FaceMean
			row.GateDance = st.DetRate >= 0.3 && st.VisMean >= 0.6 && st.FaceMean <= 0.12
		}
		row.BeatAC = ppPtr(f.BeatAC)
		row.BassRatio = ppPtr(f.BassRatio)
		row.VoiceMod = ppPtr(f.VoiceMod)
		row.PauseCnt = ppPtr(f.PauseCnt)
		row.Flatness = ppPtr(f.Flatness)
		row.Dyn = ppPtr(f.Dyn)
		rows = append(rows, row)
	}

	res := abEvaluate(rows)
	abPrintReport(res)
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
	fmt.Printf("audio-probe 完成 → %s\n", *outPath)
}

func abKey(clip string, sec int) string { return clip + "#" + strconv.Itoa(sec) }

func abSaveCache(path string, cache map[string]abFeats) {
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

// abExtractPCM 抽一窗音频：16kHz 单声道 s16le 小端。
func abExtractPCM(ffmpegBin, src string, sec, dur int) ([]float64, error) {
	if _, err := os.Stat(src); err != nil {
		return nil, err
	}
	cmd := exec.Command(ffmpegBin, "-v", "error",
		"-ss", strconv.Itoa(sec), "-t", strconv.Itoa(dur),
		"-i", src, "-vn", "-ac", "1", "-ar", strconv.Itoa(abSR),
		"-f", "s16le", "pipe:1")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	n := len(out) / 2
	pcm := make([]float64, n)
	for i := 0; i < n; i++ {
		pcm[i] = float64(int16(uint16(out[2*i])|uint16(out[2*i+1])<<8)) / 32768.0
	}
	return pcm, nil
}

// fftRadix2 就地 radix-2 FFT（re 长度=im 长度=2 的幂），返回模方谱前 N/2+1 bin。
func fftRadix2(re, im []float64) {
	n := len(re)
	for i, j := 1, 0; i < n; i++ { // 位反转置换
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wr, wi := math.Cos(ang), math.Sin(ang)
		for start := 0; start < n; start += length {
			cr, ci := 1.0, 0.0
			for k := start; k < start+length/2; k++ {
				tr := cr*re[k+length/2] - ci*im[k+length/2]
				ti := cr*im[k+length/2] + ci*re[k+length/2]
				re[k+length/2], im[k+length/2] = re[k]-tr, im[k]-ti
				re[k], im[k] = re[k]+tr, im[k]+ti
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}

// abFeatures 8s PCM → 音频特征。样本不足（<1s）返回全 NaN。
func abFeatures(pcm []float64, sr int) abFeats {
	nan := abFeats{BeatAC: math.NaN(), BassRatio: math.NaN(), VoiceMod: math.NaN(),
		PauseCnt: math.NaN(), Flatness: math.NaN(), Dyn: math.NaN()}
	if len(pcm) < sr { // <1s 不可判
		return nan
	}
	// STFT
	nFrames := (len(pcm)-abFrameSize)/abHop + 1
	nBins := abFrameSize/2 + 1
	hann := make([]float64, abFrameSize)
	for i := range hann {
		hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(abFrameSize-1))
	}
	pow := make([][]float64, nFrames) // 每帧功率谱（0..Nyquist）
	for t := 0; t < nFrames; t++ {
		re := make([]float64, abFrameSize)
		im := make([]float64, abFrameSize)
		off := t * abHop
		for i := 0; i < abFrameSize; i++ {
			re[i] = pcm[off+i] * hann[i]
		}
		fftRadix2(re, im)
		p := make([]float64, nBins)
		for k := 0; k < nBins; k++ {
			p[k] = re[k]*re[k] + im[k]*im[k]
		}
		pow[t] = p
	}
	binHz := float64(sr) / float64(abFrameSize)
	bandPow := func(lo, hi float64) []float64 { // 每帧频带功率
		k0, k1 := int(lo/binHz), int(hi/binHz)
		if k1 > nBins {
			k1 = nBins
		}
		out := make([]float64, nFrames)
		for t := range pow {
			s := 0.0
			for k := k0; k < k1; k++ {
				s += pow[t][k]
			}
			out[t] = s
		}
		return out
	}
	// 1) onset 包络（log 谱通量）
	envRate := float64(sr) / float64(abHop)
	flux := make([]float64, nFrames)
	for t := 1; t < nFrames; t++ {
		s := 0.0
		for k := 1; k < nBins; k++ {
			d := math.Log(pow[t][k]+1e-10) - math.Log(pow[t-1][k]+1e-10)
			if d > 0 {
				s += d
			}
		}
		flux[t] = s
	}
	flux[0] = 0
	// 2) beat_ac：onset 自相关在 70-180 BPM（1.17-3Hz）峰的突出度
	beatAC := abBeatAC(flux, envRate, 1.17, 3.0)
	// 3) bass_ratio：30-150Hz 功率 / 30-8kHz 功率
	bass, total := bandPow(30, 150), bandPow(30, float64(sr)/2)
	bs, ts := 0.0, 0.0
	for t := range bass {
		bs += bass[t]
		ts += total[t]
	}
	bassRatio := math.NaN()
	if ts > 0 {
		bassRatio = bs / ts
	}
	// 4) voice_mod：语音带 300-3400Hz 包络的 2-8Hz 调制能量占比
	speech := bandPow(300, 3400)
	sEnv := make([]float64, nFrames)
	for t := range speech {
		sEnv[t] = math.Sqrt(speech[t])
	}
	voiceMod := abModRatio(sEnv, envRate, 2.0, 8.0)
	// 5) pause_count：语音带包络低于中值 35% 且持续 ≥0.2s 的次数（上限 10）
	pauseCnt := float64(abPauseCount(sEnv, int(envRate), 0.35, 0.2))
	if pauseCnt > 10 {
		pauseCnt = 10
	}
	// 6) flatness：300-4kHz 谱平坦度中位数（tonal 音乐低、噪声/擦音高）
	flat := make([]float64, 0, nFrames)
	for t := range pow {
		logSum, linSum, n := 0.0, 0.0, 0
		for k := int(300 / binHz); k < int(4000/binHz) && k < nBins; k++ {
			p := pow[t][k] + 1e-12
			logSum += math.Log(p)
			linSum += p
			n++
		}
		if n > 0 && linSum > 0 {
			flat = append(flat, math.Exp(logSum/float64(n))/(linSum/float64(n)))
		}
	}
	flatness := ppMean(flat)
	// 7) dyn：全带幅度包络的变异系数
	all := make([]float64, nFrames)
	for t := range pow {
		s := 0.0
		for k := 1; k < nBins; k++ {
			s += pow[t][k]
		}
		all[t] = math.Sqrt(s)
	}
	am := ppMean(all)
	dyn := math.NaN()
	if am > 0 {
		v := 0.0
		for _, x := range all {
			v += (x - am) * (x - am)
		}
		dyn = math.Sqrt(v/float64(len(all))) / am
	}
	return abFeats{BeatAC: beatAC, BassRatio: bassRatio, VoiceMod: voiceMod,
		PauseCnt: pauseCnt, Flatness: flatness, Dyn: dyn}
}

// abBeatAC 归一化自相关在 [fLo,fHi]Hz 内最大峰的突出度（(peak-mean)/std）。
// 包络先去均值；r[0] 归一。平稳节拍（音乐）峰高，语音/噪声近随机。
func abBeatAC(env []float64, rate float64, fLo, fHi float64) float64 {
	n := len(env)
	if n < 32 {
		return math.NaN()
	}
	m := ppMean(env)
	x := make([]float64, n)
	for i := range env {
		x[i] = env[i] - m
	}
	l0, l1 := int(fLo*rate), int(fHi*rate)
	if l1 >= n-1 || l0 < 1 {
		return math.NaN()
	}
	var sum, sumsq float64
	rs := make([]float64, l1+1)
	for lag := 1; lag <= l1; lag++ {
		s := 0.0
		for i := 0; i+lag < n; i++ {
			s += x[i] * x[i+lag]
		}
		rs[lag] = s / rs0(x)
		sum += rs[lag]
		sumsq += rs[lag] * rs[lag]
	}
	cnt := float64(l1 - l0 + 1)
	mean := sum / cnt
	std := math.Sqrt(sumsq/cnt - mean*mean)
	peak := math.Inf(-1)
	for lag := l0; lag <= l1; lag++ {
		if rs[lag] > peak {
			peak = rs[lag]
		}
	}
	if std < 1e-9 {
		return 0
	}
	return (peak - mean) / std
}

// rs0 零滞后自相关（能量归一分母）。
func rs0(x []float64) float64 {
	s := 0.0
	for _, v := range x {
		s += v * v
	}
	if s == 0 {
		return 1
	}
	return s
}

// abModRatio 包络调制谱能量占比：[fLo,fHi]Hz / [0.5, Nyquist]（去 DC/漂移）。
// 语音音节调制 2-8Hz 强 → 高；连续音乐/静音 → 低。
func abModRatio(env []float64, rate float64, fLo, fHi float64) float64 {
	n := len(env)
	if n < 32 {
		return math.NaN()
	}
	m := ppMean(env)
	size := 1
	for size < n {
		size <<= 1
	}
	re := make([]float64, size)
	im := make([]float64, size)
	for i := 0; i < n; i++ {
		re[i] = env[i] - m
	}
	fftRadix2(re, im)
	binHz := rate / float64(size)
	num, den := 0.0, 0.0
	for k := 1; k < size/2; k++ {
		f := float64(k) * binHz
		p := re[k]*re[k] + im[k]*im[k]
		if f >= 0.5 {
			den += p
			if f >= fLo && f <= fHi {
				num += p
			}
		}
	}
	if den <= 0 {
		return 0
	}
	return num / den
}

// abPauseCount 包络低于 thr×中值、持续 ≥minSec 秒的运行次数。
func abPauseCount(env []float64, rate int, thr, minSec float64) int {
	if len(env) == 0 {
		return 0
	}
	med := append([]float64{}, env...)
	sort.Float64s(med)
	median := med[len(med)/2]
	if median <= 0 {
		return 0
	}
	minRun := int(minSec * float64(rate))
	if minRun < 1 {
		minRun = 1
	}
	cnt, run := 0, 0
	for _, v := range env {
		if v < thr*median {
			run++
			if run == minRun {
				cnt++
			}
		} else {
			run = 0
		}
	}
	return cnt
}

// abEvaluate 汇总：AUC（正类=dance）+ 类均值 + 否决模拟。
func abEvaluate(rows []abRow) *abResult {
	classOrder := []string{"gesture", "dance", "closeup", "chat", "none", "other"}
	byClass := map[string][]*abRow{}
	for i := range rows {
		r := &rows[i]
		byClass[r.Label] = append(byClass[r.Label], r)
	}
	feats := map[string]*abFE{}
	for _, name := range abFeatureNames {
		fe := &abFE{AUC: map[string]*float64{}, ClassMean: map[string]*float64{}, Coverage: map[string]int{}}
		vals := map[string][]float64{}
		for _, c := range classOrder {
			for _, r := range byClass[c] {
				if v := r.featPtr(name); v != nil {
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
	res := &abResult{GeneratedAt: time.Now().Format("2006/1/2 15:04:05"),
		Windows: len(rows), Features: feats, Veto: abVetoSimulation(rows), Rows: rows}
	return res
}

// abVetoSimulation 现有门判舞 ∧ 音频否决（特征缺失不否决）。
func abVetoSimulation(rows []abRow) []*abVetoOut {
	type rule struct {
		name string
		fire func(r abRow) bool
	}
	rules := []rule{
		{"voice_mod>0.45", func(r abRow) bool { return r.VoiceMod != nil && *r.VoiceMod > 0.45 }},
		{"voice_mod>0.6", func(r abRow) bool { return r.VoiceMod != nil && *r.VoiceMod > 0.6 }},
		{"pause_count>=2", func(r abRow) bool { return r.PauseCnt != nil && *r.PauseCnt >= 2 }},
		{"beat_ac<1.0", func(r abRow) bool { return r.BeatAC != nil && *r.BeatAC < 1.0 }},
		{"any(voice_mod>0.45,pause>=2)", func(r abRow) bool {
			return (r.VoiceMod != nil && *r.VoiceMod > 0.45) || (r.PauseCnt != nil && *r.PauseCnt >= 2)
		}},
		{"any(voice_mod>0.6,beat<1.0)", func(r abRow) bool {
			return (r.VoiceMod != nil && *r.VoiceMod > 0.6) || (r.BeatAC != nil && *r.BeatAC < 1.0)
		}},
	}
	gestBefore, gestTotal, danceGate := 0, 0, 0
	for i := range rows {
		switch rows[i].Label {
		case "gesture":
			gestTotal++
			if rows[i].GateDance {
				gestBefore++
			}
		case "dance":
			if rows[i].GateDance {
				danceGate++
			}
		}
	}
	out := make([]*abVetoOut, 0, len(rules))
	for _, ru := range rules {
		v := &abVetoOut{Rule: ru.name, GestureBefore: gestBefore, GestureTotal: gestTotal, DanceGate: danceGate}
		firedG := 0
		for i := range rows {
			r := rows[i]
			if !r.GateDance || !ru.fire(r) {
				continue
			}
			switch r.Label {
			case "gesture":
				firedG++
			case "dance":
				v.DanceKill++
			}
		}
		v.GestureAfter = v.GestureBefore - firedG // 剩余判舞数（曾误存被否决数导致语义反转）
		out = append(out, v)
	}
	return out
}

func abPrintReport(res *abResult) {
	fmt.Printf("\n窗数: %d\n", res.Windows)
	fmt.Printf("\n逐特征 AUC（正类=dance，方向未定，0.5=无信号）:\n")
	fmt.Printf("  %-12s %11s %11s %11s %11s | 覆盖 g/d\n", "feature", "vs_gesture", "vs_closeup", "vs_chat", "vs_none")
	for _, name := range abFeatureNames {
		e := res.Features[name]
		cell := func(k string) string {
			if v, ok := e.AUC[k]; ok && v != nil {
				return fmt.Sprintf("%.3f", *v)
			}
			return "—"
		}
		fmt.Printf("  %-12s %11s %11s %11s %11s | %d/%d\n", name,
			cell("vs_gesture"), cell("vs_closeup"), cell("vs_chat"), cell("vs_none"),
			e.Coverage["gesture"], e.Coverage["dance"])
	}
	fmt.Printf("\n类均值:\n")
	for _, name := range abFeatureNames {
		e := res.Features[name]
		cell := func(c string) string {
			if v, ok := e.ClassMean[c]; ok && v != nil {
				return fmt.Sprintf("%.3f", *v)
			}
			return "—"
		}
		fmt.Printf("  %-12s gesture=%-9s dance=%-9s closeup=%-9s chat=%-9s none=%s\n",
			name, cell("gesture"), cell("dance"), cell("closeup"), cell("chat"), cell("none"))
	}
	fmt.Printf("\n否决规则模拟（门判舞 ∧ 音频否决；gesture/dance 为本探针金标）:\n")
	for _, v := range res.Veto {
		before, after := 0.0, 0.0
		if v.GestureTotal > 0 {
			before = 100 * float64(v.GestureBefore) / float64(v.GestureTotal)
			after = 100 * float64(v.GestureAfter) / float64(v.GestureTotal)
		}
		kill := 0.0
		if v.DanceGate > 0 {
			kill = 100 * float64(v.DanceKill) / float64(v.DanceGate)
		}
		fmt.Printf("  %-28s gesture 判舞 %d/%d(%.1f%%)→%d(%.1f%%) | 舞误杀 %d/%d(%.1f%%)\n",
			v.Rule, v.GestureBefore, v.GestureTotal, before, v.GestureAfter, after,
			v.DanceKill, v.DanceGate, kill)
	}
}
