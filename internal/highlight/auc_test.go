package highlight

import (
	"math"
	"testing"
)

func TestMannWhitneyAUCPerfectSeparation(t *testing.T) {
	scores := []float64{0.1, 0.2, 0.8, 0.9}
	labels := []int{0, 0, 1, 1}
	if a := MannWhitneyAUC(scores, labels); math.Abs(a-1) > 1e-9 {
		t.Fatalf("AUC=%v want 1", a)
	}
}

func TestMannWhitneyAUCInverted(t *testing.T) {
	scores := []float64{0.9, 0.8, 0.2, 0.1}
	labels := []int{0, 0, 1, 1}
	if a := MannWhitneyAUC(scores, labels); math.Abs(a) > 1e-9 {
		t.Fatalf("AUC=%v want 0", a)
	}
}

func TestMannWhitneyAUCTies(t *testing.T) {
	scores := []float64{1, 1, 1, 1}
	labels := []int{0, 1, 0, 1}
	if a := MannWhitneyAUC(scores, labels); math.Abs(a-0.5) > 1e-9 {
		t.Fatalf("AUC=%v want 0.5", a)
	}
}

func TestWindowBStdP90LocalHot(t *testing.T) {
	// 前 10 秒均匀，后 10 秒局部热 → p90 应接近局部 std
	b := blocksFrom(
		[9]float64{1, 1, 1, 1, 1, 1, 1, 1, 1},
		[9]float64{0, 0, 0, 0, 20, 0, 0, 0, 0},
	)
	if got := WindowBStdP90(b, 0, 2); got < 3 {
		t.Fatalf("p90=%v want high", got)
	}
}
