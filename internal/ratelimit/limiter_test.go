package ratelimit

import (
	"testing"
	"time"
)

func TestSelectDayNight(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	night := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if Select(20, 80, day) != 20 {
		t.Fatal("daytime should use dayRate")
	}
	if Select(20, 80, night) != 80 {
		t.Fatal("night should use nightRate")
	}
	edge := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	if Select(20, 80, edge) != 80 {
		t.Fatal("23:00 is night")
	}
}
