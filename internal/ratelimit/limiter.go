// Package ratelimit 提供按日/夜时段切换的上传限速（单位 MB/s）。
package ratelimit

import "time"

// DayWindow 是白班限速时段 [Start, End) 小时。
const (
	DayStartHour = 8
	DayEndHour   = 23
)

// Select 根据当前小时选择限速值（MB/s）。
func Select(dayRate, nightRate int, now time.Time) int {
	h := now.Hour()
	if h >= DayStartHour && h < DayEndHour {
		return dayRate
	}
	return nightRate
}
