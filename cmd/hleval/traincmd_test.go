package main

import (
	"math"
	"testing"
)

// smoothPy 必须与 numpy.convolve(x, ones(w)/w, mode="same") 逐点一致：
// 两端零填充、除以完整窗口宽（边缘衰减）。
func TestSmoothPyMatchesNumpyConvolve(t *testing.T) {
	x := []float64{1, 2, 3, 4, 5}
	got := smoothPy(x, 5)
	want := []float64{1.2, 2.0, 3.0, 2.8, 2.4} // (1+2+3)/5, (1+2+3+4)/5, 15/5, (2+3+4+5)/5, (3+4+5)/5
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("smoothPy[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// 恒定输入在 convolve 零填充语义下边缘被衰减 → z 分非 0（与 Python 完全一致，
// 数值为手工按 numpy 语义算出的精确值）；长度 2 时平滑结果恒定 → 回退全 0。
func TestNormSlicewisePyConstant(t *testing.T) {
	got := normSlicewisePy([]float64{3, 3, 3, 3, 3, 3})
	// sm = [1.8,2.4,3,3,2.4,1.8]; med=2.4; mad=0.6; scale=1.4826*0.6
	scale := 1.4826 * 0.6
	if math.Abs(got[0]-(1.8-2.4)/scale) > 1e-12 {
		t.Fatalf("normSlicewisePy[0] = %v, want %v", got[0], (1.8-2.4)/scale)
	}
	if math.Abs(got[2]-(3-2.4)/scale) > 1e-12 {
		t.Fatalf("normSlicewisePy[2] = %v, want %v", got[2], (3-2.4)/scale)
	}
	zeros := normSlicewisePy([]float64{3, 3})
	for i, v := range zeros {
		if v != 0 {
			t.Fatalf("n=2 恒定输入应回退全 0: [%d]=%v", i, v)
		}
	}
}

// 线性可分二维数据上，逻辑回归必须把训练集分对（凸问题，Newton 应精确收敛）。
func TestFitLogregSeparable(t *testing.T) {
	var X [][]float64
	var y []int
	for i := 0; i < 50; i++ {
		X = append(X, []float64{float64(i) / 10, 1.0})
		y = append(y, 1)
		X = append(X, []float64{float64(i) / 10, -1.0})
		y = append(y, 0)
	}
	w, b := fitLogreg(X, y, 0.5, 100)
	if w[1] <= 0 {
		t.Fatalf("第二维系数应为正（该维度单独可分）: %v", w)
	}
	// 训练集准确率应为 100%
	for i := range X {
		z := b + w[0]*X[i][0] + w[1]*X[i][1]
		pred := 0
		if sigmoid(z) > 0.5 {
			pred = 1
		}
		if pred != y[i] {
			t.Fatalf("样本 %d (x=%v) 分错: pred=%d want=%d", i, X[i], pred, y[i])
		}
	}
}

// class_weight=balanced：正负样本 1:9 时，少数类权重应放大 9 倍。
func TestFitLogregBalancedWeightsDirection(t *testing.T) {
	var X [][]float64
	var y []int
	for i := 0; i < 90; i++ {
		X = append(X, []float64{-1})
		y = append(y, 0)
	}
	for i := 0; i < 10; i++ {
		X = append(X, []float64{1})
		y = append(y, 1)
	}
	w, _ := fitLogreg(X, y, 1e6, 100) // 近似无正则
	if w[0] <= 0 {
		t.Fatalf("balanced 加权下少数类方向系数应为正: %v", w)
	}
}

func TestMedianF64(t *testing.T) {
	if got := medianF64([]float64{3, 1, 2}); got != 2 {
		t.Fatalf("median odd = %v, want 2", got)
	}
	if got := medianF64([]float64{4, 1, 2, 3}); got != 2.5 {
		t.Fatalf("median even = %v, want 2.5", got)
	}
}

func TestMetricsPR(t *testing.T) {
	p, r, f1, tp, fp, fn := metricsPR([]int{1, 0, 1, 0}, []int{1, 1, 0, 0})
	if tp != 1 || fp != 1 || fn != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/1", tp, fp, fn)
	}
	if math.Abs(p-0.5) > 1e-12 || math.Abs(r-0.5) > 1e-12 || math.Abs(f1-0.5) > 1e-12 {
		t.Fatalf("metrics = %v/%v/%v, want 0.5/0.5/0.5", p, r, f1)
	}
}
