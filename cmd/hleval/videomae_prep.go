// videomae 预处理纯函数：与 cgo（ORT 推理）解耦，保证纯 Go 构建下也能测试。
//
// 帧口径与 _videomae_embed.py 逐字对齐：每窗 8 帧（f_{sec+1}..f_{sec+8}，1fps）
// → 逐帧×2 成 16 帧（tubelet2 位置嵌入几何不变）；letterbox 224（128 灰）；
// normalize (x/255-0.5)/0.5。
package main

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"math"
	"os"
	"sort"
	"strings"
)

const (
	vmSize   = 224
	vmFrames = 16 // 8 帧 ×2（VideoMAE 预训练 16帧×tubelet2）
	vmFill   = float32(128.0/255.0*2 - 1)
)

// vmWindowTensor：8 帧 → letterbox 归一化 [3,224,224] → ×2 复制成 [16,3,224,224] NCHW。
func vmWindowTensor(framesRoot, clip string, sec int) ([]float32, error) {
	per := make([][]float32, 8)
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("%s/%s/f_%04d.jpg", framesRoot, clip, sec+i+1)
		img, err := decodeJPEG(p)
		if err != nil {
			return nil, fmt.Errorf("videomae: %w", err)
		}
		per[i] = vmPreprocess(img)
	}
	out := make([]float32, 0, vmFrames*3*vmSize*vmSize)
	for i := 0; i < 8; i++ { // f0,f0,f1,f1,… 与 Python f8+f8 一致
		out = append(out, per[i]...)
		out = append(out, per[i]...)
	}
	return out, nil
}

func decodeJPEG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// vmPreprocess：letterbox 224（128 灰填充）+ normalize (x/255-0.5)/0.5 → CHW。
// 缩小（s<1）走面积加权 box（抗混叠，近似 PIL 的滤波重采样）；放大走双线性。
// 不引入 golang.org/x/image 依赖。
func vmPreprocess(img image.Image) []float32 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	s := math.Min(float64(vmSize)/float64(w), float64(vmSize)/float64(h))
	nw, nh := max(1, int(math.Round(float64(w)*s))), max(1, int(math.Round(float64(h)*s)))
	ox, oy := (vmSize-nw)/2, (vmSize-nh)/2

	out := make([]float32, 3*vmSize*vmSize)
	for i := range out {
		out[i] = vmFill
	}
	if s < 1 { // 缩小：dst 像素 = src 覆盖矩形重叠面积加权平均（box）
		for y := 0; y < nh; y++ {
			sy0, sy1 := float64(y)/s, float64(y+1)/s
			for x := 0; x < nw; x++ {
				sx0, sx1 := float64(x)/s, float64(x+1)/s
				var r, g, bl, wsum float64
				for py := max(0, int(sy0)); py < min(h, int(math.Ceil(sy1))); py++ {
					wy := vmOverlap(sy0, sy1, float64(py), float64(py+1))
					if wy <= 0 {
						continue
					}
					for px := max(0, int(sx0)); px < min(w, int(math.Ceil(sx1))); px++ {
						wx := vmOverlap(sx0, sx1, float64(px), float64(px+1))
						if wx <= 0 {
							continue
						}
						rr, gg, bb, _ := img.At(b.Min.X+px, b.Min.Y+py).RGBA()
						wt := wx * wy
						r += float64(rr>>8) * wt
						g += float64(gg>>8) * wt
						bl += float64(bb>>8) * wt
						wsum += wt
					}
				}
				if wsum == 0 {
					continue
				}
				base := (y+oy)*vmSize + (x + ox)
				out[base] = float32(r/wsum/255*2 - 1)
				out[vmSize*vmSize+base] = float32(g/wsum/255*2 - 1)
				out[2*vmSize*vmSize+base] = float32(bl/wsum/255*2 - 1)
			}
		}
		return out
	}
	// 放大/等比：双线性
	for y := 0; y < nh; y++ {
		sy := (float64(y)+0.5)/s - 0.5
		y0, y1, fy := vmSampleIdx(sy, h)
		for x := 0; x < nw; x++ {
			sx := (float64(x)+0.5)/s - 0.5
			x0, x1, fx := vmSampleIdx(sx, w)
			r00, g00, b00, _ := img.At(b.Min.X+x0, b.Min.Y+y0).RGBA()
			r10, g10, b10, _ := img.At(b.Min.X+x1, b.Min.Y+y0).RGBA()
			r01, g01, b01, _ := img.At(b.Min.X+x0, b.Min.Y+y1).RGBA()
			r11, g11, b11, _ := img.At(b.Min.X+x1, b.Min.Y+y1).RGBA()
			base := (y+oy)*vmSize + (x + ox)
			out[base] = vmChan(r00, r10, r01, r11, fx, fy)
			out[vmSize*vmSize+base] = vmChan(g00, g10, g01, g11, fx, fy)
			out[2*vmSize*vmSize+base] = vmChan(b00, b10, b01, b11, fx, fy)
		}
	}
	return out
}

// vmOverlap：[a1,a2) 与 [b1,b2) 的重叠长度。
func vmOverlap(a1, a2, b1, b2 float64) float64 {
	lo, hi := math.Max(a1, b1), math.Min(a2, b2)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

func vmSampleIdx(s float64, limit int) (int, int, float64) {
	f := math.Floor(s)
	i := int(f)
	frac := s - f
	i0, i1 := i, i+1
	if i0 < 0 {
		i0, i1, frac = 0, 0, 0
	}
	if i1 > limit-1 {
		i1 = limit - 1
	}
	if i0 > limit-1 {
		i0 = limit - 1
	}
	return i0, i1, frac
}

func vmChan(c00, c10, c01, c11 uint32, fx, fy float64) float32 {
	a := float64(c00>>8)*(1-fx) + float64(c10>>8)*fx
	b := float64(c01>>8)*(1-fx) + float64(c11>>8)*fx
	v := (a*(1-fy)+b*fy)/255*2 - 1
	return float32(v)
}

func vmNorm(v []float32) float64 {
	s := 0.0
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func vmMean(v []float32) float64 {
	s := 0.0
	for _, x := range v {
		s += float64(x)
	}
	return s / float64(len(v))
}

func vmCosine(a, b []float32) float64 {
	dot, na, nb := 0.0, 0.0, 0.0
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func vmMaxAbsDiff(a, b []float32) float64 {
	m := 0.0
	for i := range a {
		d := math.Abs(float64(a[i]) - float64(b[i]))
		if d > m {
			m = d
		}
	}
	return m
}

// vmTop5：logits softmax 后取 top5（label 表来自 K400 config.json 的 id2label）。
func vmTop5(configPath string, logits []float32) ([]map[string]string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("videomae: 读 labels: %w", err)
	}
	var cfg struct {
		ID2Label map[string]string `json:"id2label"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("videomae: 解析 labels: %w", err)
	}
	mx := float32(math.Inf(-1))
	for _, v := range logits {
		if v > mx {
			mx = v
		}
	}
	sum := 0.0
	exps := make([]float64, len(logits))
	for i, v := range logits {
		exps[i] = math.Exp(float64(v - mx))
		sum += exps[i]
	}
	type kv struct {
		id int
		p  float64
	}
	ord := make([]kv, len(logits))
	for i := range ord {
		ord[i] = kv{i, exps[i] / sum}
	}
	sort.Slice(ord, func(a, b int) bool { return ord[a].p > ord[b].p })
	out := make([]map[string]string, 0, 5)
	for _, k := range ord[:5] {
		out = append(out, map[string]string{
			"label": strings.TrimSpace(cfg.ID2Label[fmt.Sprint(k.id)]),
			"prob":  fmt.Sprintf("%.4f", k.p),
		})
	}
	return out, nil
}
