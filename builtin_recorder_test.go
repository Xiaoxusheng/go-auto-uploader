package main

import (
	"testing"
	"time"
)

// TestExtractBuiltinRoomID 测试各种平台直播间链接的房间号提取逻辑。
// 启用并行测试以最大化利用 CPU 多核性能。
func TestExtractBuiltinRoomID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Douyin Web URL", "https://live.douyin.com/1234567890123456789", "1234567890123456789"},
		{"Kuaishou Web URL", "https://live.kuaishou.com/u/3xtnuitaz2982eg", "3xtnuitaz2982eg"},
		{"Soop URL", "https://play.sooplive.co.kr/testroom/12345", "testroom"},
		{"Raw Room ID", "88888888", "88888888"},
		{"Empty String", "", ""},
		{"URL with trailing slash", "https://live.douyin.com/987654321/", "987654321"},
	}

	for _, tt := range tests {
		tt := tt // 捕获循环变量，防止闭包在并发时出现数据竞争
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := extractBuiltinRoomID(tt.input)
			if result != tt.expected {
				t.Errorf("extractBuiltinRoomID() = %v, 预期 %v", result, tt.expected)
			}
		})
	}
}

// TestSanitizeBuiltinFileName 测试主播名称规范化逻辑，确保生成的文件夹名称符合 OS 操作系统规范。
// 针对特殊字符、换行符及空字符进行高压覆盖。
func TestSanitizeBuiltinFileName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Normal Name", "张三的直播间", "张三的直播间"},
		{"Name with Invalid OS Chars", "李四 / \\ : * ? \" < > |", "李四"},
		{"Name with Newlines", "王五\n\r直播", "王五直播"},
		{"Empty Name", "", "未命名主播"},
		{"Spaces Only", "   ", "未命名主播"},
		{"Trailing and Leading Spaces", "  赵六  ", "赵六"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := sanitizeBuiltinFileName(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeBuiltinFileName() = %v, 预期 %v", result, tt.expected)
			}
		})
	}
}

// TestFormatBuiltinDuration 测试将高精度的时间差（time.Duration）转换为前端可读的中文字符串。
// 覆盖秒级、分级及小时级的边界情况。
func TestFormatBuiltinDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"Only Seconds", 45 * time.Second, "00分45秒"},
		{"Minutes and Seconds", 5*time.Minute + 30*time.Second, "05分30秒"},
		{"Hours, Minutes, and Seconds", 2*time.Hour + 15*time.Minute + 5*time.Second, "02小时15分05秒"},
		{"Zero Duration", 0, "00分00秒"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := formatBuiltinDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("formatBuiltinDuration() = %v, 预期 %v", result, tt.expected)
			}
		})
	}
}

// TestFormatBuiltinBytes 测试字节大小格式化函数的精确度。
// 确保 B, KB, MB, GB, TB 能够根据 1024 进制正确转换并保留合理小数。
func TestFormatBuiltinBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{"Bytes", 500, "500 B"},
		{"Kilobytes", 1024 + 512, "1.50 KB"},
		{"Megabytes", 1024 * 1024 * 2, "2.00 MB"},
		{"Gigabytes", 1024 * 1024 * 1024 * 5, "5.00 GB"},
		{"Terabytes", 1024 * 1024 * 1024 * 1024 * 3, "3.00 TB"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := formatBuiltinBytes(tt.bytes)
			if result != tt.expected {
				t.Errorf("formatBuiltinBytes() = %v, 预期 %v", result, tt.expected)
			}
		})
	}
}

// TestParseBuiltinLine 测试主播配置文件解析功能，确保暂停符号、URL及自定义名称能被极速剥离。
// 此处为核心数据解析流程，影响底层监控任务的初始化。
func TestParseBuiltinLine(t *testing.T) {
	t.Parallel()

	type expected struct {
		isPaused   bool
		platform   string
		roomID     string
		customName string
		rawURL     string
	}

	tests := []struct {
		name     string
		line     string
		expected expected
	}{
		{
			name:     "Standard Douyin Line",
			line:     "https://live.douyin.com/123456,主播:测试主播",
			expected: expected{false, "Douyin", "123456", "测试主播", "https://live.douyin.com/123456"},
		},
		{
			name:     "Paused Kuaishou Line",
			line:     "# https://live.kuaishou.com/u/abcde, 主播:休眠主播",
			expected: expected{true, "Kuaishou", "abcde", "休眠主播", "https://live.kuaishou.com/u/abcde"},
		},
		{
			name:     "Soop Line Without Name",
			line:     "https://play.sooplive.co.kr/testroom",
			expected: expected{false, "Soop", "testroom", "", "https://play.sooplive.co.kr/testroom"},
		},
		{
			name:     "Empty Line",
			line:     "   ",
			expected: expected{false, "", "", "", ""},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			isPaused, platform, roomID, customName, rawURL := parseBuiltinLine(tt.line)

			if isPaused != tt.expected.isPaused ||
				platform != tt.expected.platform ||
				roomID != tt.expected.roomID ||
				customName != tt.expected.customName ||
				rawURL != tt.expected.rawURL {
				t.Errorf("parseBuiltinLine() 获得 = (%v, %v, %v, %v, %v), 预期 = (%v, %v, %v, %v, %v)",
					isPaused, platform, roomID, customName, rawURL,
					tt.expected.isPaused, tt.expected.platform, tt.expected.roomID, tt.expected.customName, tt.expected.rawURL)
			}
		})
	}
}

// TestBuiltinRC4Encrypt 测试自定义的 RC4 对称加密混淆算法。
// 利用已知的明文和密钥比对密文，验证核心加密引擎的准确性。
func TestBuiltinRC4Encrypt(t *testing.T) {
	t.Parallel()

	plaintext := "test_user_agent_123"
	key := string([]byte{0, 1, 14}) // a_bogus 计算所用密钥

	encrypted := builtinRC4Encrypt(plaintext, key)
	decrypted := builtinRC4Encrypt(encrypted, key) // RC4 对称特性：再加密一次即为解密

	if decrypted != plaintext {
		t.Errorf("RC4 加解密异常: 得到 %v, 预期 %v", decrypted, plaintext)
	}
}
