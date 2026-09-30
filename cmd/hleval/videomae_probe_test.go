package main

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// TestVMPreprocessDimsFill：纯色 480×848 帧 → letterbox 224 后尺寸正确、
// 填充区值 = (128/255·2-1)、实画面区不在填充值（内容被写入）。
func TestVMPreprocessDimsFill(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 480, 848))
	for y := 0; y < 848; y++ {
		for x := 0; x < 480; x++ {
			src.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	got := vmPreprocess(src)
	if len(got) != 3*vmSize*vmSize {
		t.Fatalf("长度 = %d, 期望 %d", len(got), 3*vmSize*vmSize)
	}
	wantFill := float32(128.0/255.0*2 - 1)
	corners := []int{0, vmSize - 1, vmSize*vmSize - 1} // 左上/右上/右下角（竖屏 letterbox 必为灰边）
	for _, c := range []int{0, 1, 2} {
		for _, off := range corners {
			if v := got[c*vmSize*vmSize+off]; math.Abs(float64(v-wantFill)) > 1e-4 {
				t.Fatalf("通道 %d 角落 %d = %v, 期望填充 %v", c, off, v, wantFill)
			}
		}
	}
	// 画面中心（横向居中条带内）应有内容：中心点应接近 (200,100,50) 归一化值
	mid := vmSize/2*vmSize + vmSize/2
	wantR := float32(200.0/255*2 - 1)
	if math.Abs(float64(got[mid]-wantR)) > 0.05 {
		t.Fatalf("中心 R = %v, 期望 ≈%v（双线性容差 0.05）", got[mid], wantR)
	}
}

// TestVMSampleIdx：采样下标在边界处的钳制。
func TestVMSampleIdx(t *testing.T) {
	if a, b, f := vmSampleIdx(-0.6, 100); a != 0 || b != 0 || f != 0 {
		t.Fatalf("负下标: %d %d %v", a, b, f)
	}
	if a, b, f := vmSampleIdx(99.4, 100); a != 99 || b != 99 {
		t.Fatalf("上界: %d %d %v", a, b, f)
	}
	if a, b, f := vmSampleIdx(10.5, 100); a != 10 || b != 11 || math.Abs(f-0.5) > 1e-9 {
		t.Fatalf("普通: %d %d %v", a, b, f)
	}
}

// TestVMCosine：余弦函数自检（同向=1、正交=0）。
func TestVMCosine(t *testing.T) {
	a := []float32{1, 2, 3}
	if c := vmCosine(a, a); math.Abs(c-1) > 1e-9 {
		t.Fatalf("自余弦 = %v", c)
	}
	if c := vmCosine([]float32{1, 0}, []float32{0, 1}); math.Abs(c) > 1e-9 {
		t.Fatalf("正交 = %v", c)
	}
}
