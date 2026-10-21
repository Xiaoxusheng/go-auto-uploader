package httpapi

import (
	"math"
	"testing"
)

// 分页参数全部来自 query，属不可信输入。
//
// 原实现 start := (page-1)*limit 在 page 取极大值时整数溢出成负数，
// 随后 filtered[负数:...] 直接 panic。pageRange 必须先按总页数夹住 page。
func TestPageRangeNoOverflow(t *testing.T) {
	cases := []struct {
		name        string
		page, limit int
		total       int
		wantOK      bool
		wantStart   int
		wantEnd     int
	}{
		{"空列表", 1, 50, 0, false, 0, 0},
		{"首页", 1, 50, 10, true, 0, 10},
		{"第二页越界", 2, 50, 10, false, 0, 0},
		{"末页截断", 2, 4, 10, true, 4, 8},
		{"page=MaxInt 不得溢出", math.MaxInt, 500, 10, false, 0, 0},
		{"page=MaxInt64/2", math.MaxInt64 / 2, 500, 10, false, 0, 0},
		{"limit 非法", 1, 0, 10, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, ok := pageRange(c.page, c.limit, c.total)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (start != c.wantStart || end != c.wantEnd) {
				t.Fatalf("range = [%d,%d), want [%d,%d)", start, end, c.wantStart, c.wantEnd)
			}
		})
	}
}

func TestParsePageClamps(t *testing.T) {
	page, limit := parsePage("0", "999999")
	if page != 1 {
		t.Fatalf("page = %d, want 1", page)
	}
	if limit != maxPageLimit {
		t.Fatalf("limit = %d, want %d", limit, maxPageLimit)
	}
	if p, l := parsePage("abc", ""); p != 1 || l != 50 {
		t.Fatalf("非法参数应回落 1/50，得到 %d/%d", p, l)
	}
}
