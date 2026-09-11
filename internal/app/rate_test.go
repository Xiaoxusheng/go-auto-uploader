package app

import (
	"testing"
	"time"

	"upload/internal/config"
)

func TestCurrentRateManualOverride(t *testing.T) {
	CfgStore.Replace(config.Config{
		Rate:      15,
		DayRate:   20,
		NightRate: 80,
	})
	defer CfgStore.Replace(config.Config{})

	if got := CurrentRate(); got != 15 {
		t.Fatalf("手动 Rate=15 应覆盖日夜表, got %d", got)
	}
}

func TestCurrentRateFallsBackToDayNight(t *testing.T) {
	day, night := 20, 80
	CfgStore.Replace(config.Config{DayRate: day, NightRate: night})
	defer CfgStore.Replace(config.Config{})

	got := CurrentRate()
	h := time.Now().Hour()
	want := night
	if h >= 8 && h < 23 {
		want = day
	}
	if got != want {
		t.Fatalf("Rate=0 时应走日夜表, got %d want %d", got, want)
	}
}
