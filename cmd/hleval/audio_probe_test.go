package main

import (
	"math"
	"math/rand"
	"testing"
)

// TestFFTRadix2Correctness 与直接 DFT 对比（N=8 随机信号）。
func TestFFTRadix2Correctness(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	n := 8
	re := make([]float64, n)
	im := make([]float64, n)
	sig := make([]float64, n)
	for i := 0; i < n; i++ {
		re[i] = rng.Float64()*2 - 1
		sig[i] = re[i]
	}
	fftRadix2(re, im)
	for k := 0; k < n; k++ {
		sr, si := 0.0, 0.0
		for t := 0; t < n; t++ {
			ang := -2 * math.Pi * float64(k) * float64(t) / float64(n)
			sr += sig[t] * math.Cos(ang)
			si += sig[t] * math.Sin(ang)
		}
		if math.Abs(re[k]-sr) > 1e-9 || math.Abs(im[k]-si) > 1e-9 {
			t.Fatalf("FFT 与 DFT 不一致 at k=%d: got (%v,%v) want (%v,%v)", k, re[k], im[k], sr, si)
		}
	}
}

// TestFFTParseval 能量守恒（Parseval）。
func TestFFTParseval(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	n := 16
	re := make([]float64, n)
	im := make([]float64, n)
	timeEnergy := 0.0
	for i := 0; i < n; i++ {
		re[i] = rng.NormFloat64()
		timeEnergy += re[i] * re[i]
	}
	fftRadix2(re, im)
	freqEnergy := 0.0
	for k := 0; k < n; k++ {
		freqEnergy += re[k]*re[k] + im[k]*im[k]
	}
	if math.Abs(freqEnergy/float64(n)-timeEnergy) > 1e-6*math.Max(1, timeEnergy) {
		t.Fatalf("Parseval 不守恒: time=%v freq/n=%v", timeEnergy, freqEnergy/float64(n))
	}
}

// synthMusic 合成「音乐」：2.2Hz 节拍 click + 60Hz 低音持续音 + 440Hz 和声。
func synthMusic(sr int, sec float64) []float64 {
	n := int(float64(sr) * sec)
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sr)
		v := 0.5 * math.Sin(2*math.Pi*60*t)              // 低音
		v += 0.2 * math.Sin(2*math.Pi*440*t)             // 和声
		if phase := math.Mod(t*2.2, 1.0); phase < 0.02 { // 2.2Hz click
			v += 0.8 * (1 - phase/0.02)
		}
		out[i] = v
	}
	return out
}

// synthSpeech 合成「说话」：300-3400Hz 带内 4Hz 音节爆发 + 停顿。
func synthSpeech(sr int, sec float64) []float64 {
	n := int(float64(sr) * sec)
	out := make([]float64, n)
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sr)
		// 4Hz 音节包络，且每秒前 0.7s 有声（后 0.3s 停顿）
		syl := 0.5 + 0.5*math.Sin(2*math.Pi*4*t)
		talking := math.Mod(t, 1.0) < 0.7
		v := 0.0
		if talking {
			v = syl * (0.3*math.Sin(2*math.Pi*500*t) + 0.3*math.Sin(2*math.Pi*1200*t) +
				0.2*rng.NormFloat64())
		}
		out[i] = v
	}
	return out
}

// TestAbFeaturesDirection 合成信号方向性：音乐 beat_ac 高 / bass_ratio 高；
// 说话 voice_mod 高、pause_count≥1。
func TestAbFeaturesDirection(t *testing.T) {
	fm := abFeatures(synthMusic(16000, 8), 16000)
	fs := abFeatures(synthSpeech(16000, 8), 16000)
	if math.IsNaN(fm.BeatAC) || math.IsNaN(fs.BeatAC) {
		t.Fatalf("beat_ac 意外 NaN: music=%v speech=%v", fm.BeatAC, fs.BeatAC)
	}
	if !(fm.BeatAC > fs.BeatAC) {
		t.Fatalf("beat_ac 方向错: music=%.3f speech=%.3f（应 music>speech）", fm.BeatAC, fs.BeatAC)
	}
	if !(fm.BassRatio > fs.BassRatio) {
		t.Fatalf("bass_ratio 方向错: music=%.3f speech=%.3f（应 music>speech）", fm.BassRatio, fs.BassRatio)
	}
	if !(fs.VoiceMod > fm.VoiceMod) {
		t.Fatalf("voice_mod 方向错: speech=%.3f music=%.3f（应 speech>music）", fs.VoiceMod, fm.VoiceMod)
	}
	if fs.PauseCnt < 1 {
		t.Fatalf("speech pause_count 应≥1，得 %v", fs.PauseCnt)
	}
	if math.IsNaN(fs.PauseCnt) || math.IsNaN(fs.VoiceMod) {
		t.Fatal("speech 特征不应 NaN")
	}
}

// TestAbFeaturesTooShort 样本不足全 NaN。
func TestAbFeaturesTooShort(t *testing.T) {
	f := abFeatures(make([]float64, 8000), 16000) // 0.5s
	for _, name := range abFeatureNames {
		if !math.IsNaN(f.get(name)) {
			t.Fatalf("%s 应 NaN，得 %v", name, f.get(name))
		}
	}
}

// TestAbPauseCount 停顿计数逻辑。
func TestAbPauseCount(t *testing.T) {
	env := make([]float64, 100)
	for i := range env {
		env[i] = 1.0
		if (i >= 20 && i < 30) || (i >= 60 && i < 72) { // 两个低于阈值的谷
			env[i] = 0.1
		}
	}
	// rate=100/s，minSec=0.05（5 样本）→ 两个谷各持续 10/12 样本 → 2 次
	if got := abPauseCount(env, 100, 0.35, 0.05); got != 2 {
		t.Fatalf("pause_count 应 2，得 %d", got)
	}
}
