package config

import (
	"reflect"
	"testing"
)

// 存储溢出护栏配置归一化：trim/去空/Clean/去重/去与主目录重复；阈值缺省与钳制。
func TestBuiltinStorageFallbackDefaults(t *testing.T) {
	b := BuiltinSettings{
		SavePath:          "./downloads",
		SavePathFallbacks: []string{"  D:\\rec_a  ", "", "D:\\rec_a", "./downloads", "D:\\rec_b"},
		MinFreeGB:         0,
	}
	b.ApplyDefaults()
	// Clean("./downloads") 与 Clean("./downloads") 相同被剔除；重复项去重；空白项丢弃
	want := []string{"D:\\rec_a", "D:\\rec_b"}
	if !reflect.DeepEqual(b.SavePathFallbacks, want) {
		t.Fatalf("SavePathFallbacks = %v, want %v", b.SavePathFallbacks, want)
	}
	if b.MinFreeGB != 10 {
		t.Fatalf("配置了备选目录时 MinFreeGB 缺省应为 10, got %v", b.MinFreeGB)
	}

	// 未配置备选目录：功能关闭，阈值不补缺省
	b2 := BuiltinSettings{}
	b2.ApplyDefaults()
	if len(b2.SavePathFallbacks) != 0 {
		t.Fatalf("未配置备选目录应保持为空, got %v", b2.SavePathFallbacks)
	}
	if b2.MinFreeGB != 0 {
		t.Fatalf("未配置备选目录时 MinFreeGB 不应补缺省, got %v", b2.MinFreeGB)
	}

	// 显式阈值保留；无备选目录时负值钳到 0
	b3 := BuiltinSettings{SavePath: "D:\\x", SavePathFallbacks: []string{"D:\\fb"}, MinFreeGB: 25}
	b3.ApplyDefaults()
	if b3.MinFreeGB != 25 {
		t.Fatalf("显式阈值应保留 25, got %v", b3.MinFreeGB)
	}
	b4 := BuiltinSettings{MinFreeGB: -5}
	b4.ApplyDefaults()
	if b4.MinFreeGB != 0 {
		t.Fatalf("负值阈值应钳到 0, got %v", b4.MinFreeGB)
	}
}
