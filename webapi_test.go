package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TestEncryptDecryptPayload 测试核心的 AES-GCM 动态加密与解密流程。
// 确保在并发环境下，载荷的加解密能够绝对还原，且对非法密文/非法密钥具备正确的防范拦截能力。
func TestEncryptDecryptPayload(t *testing.T) {
	t.Parallel()

	// 生成随机的 32 字节 (256-bit) AES 密钥
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("无法生成测试密钥: %v", err)
	}

	originalPayload := []byte(`{"username":"admin","action":"start_engine"}`)

	// 测试 1: 正常加密流程
	encryptedBase64, err := encryptPayload(originalPayload, key)
	if err != nil {
		t.Fatalf("加密过程发生异常: %v", err)
	}
	if encryptedBase64 == "" {
		t.Fatal("加密结果为空字符串")
	}

	// 测试 2: 正常解密流程
	decryptedPayload, err := decryptPayload(encryptedBase64, key)
	if err != nil {
		t.Fatalf("解密过程发生异常: %v", err)
	}
	if !bytes.Equal(originalPayload, decryptedPayload) {
		t.Errorf("解密后的数据与原数据不一致! 预期 %s, 获得 %s", originalPayload, decryptedPayload)
	}

	// 测试 3: 篡改密文拦截测试 (破坏 Base64 结构)
	_, err = decryptPayload(encryptedBase64[:len(encryptedBase64)-2]+"==", key)
	if err == nil {
		t.Error("安全漏洞：被篡改的密文应当解密失败，但却成功了")
	}

	// 测试 4: 错误密钥拦截测试
	wrongKey := make([]byte, 32)
	rand.Read(wrongKey)
	_, err = decryptPayload(encryptedBase64, wrongKey)
	if err == nil {
		t.Error("安全漏洞：使用错误的 AES 密钥应当解密失败，但却成功了")
	}
}

// TestRestoreQueueCounts 测试基于无锁化 sync.Map 和 atomic 的队列计数器恢复逻辑。
// 验证在系统发生热重载或崩溃恢复时，能否精确、无遗漏地统计算出当前所有队列的任务积压量。
func TestRestoreQueueCounts(t *testing.T) {
	// 清理全局状态，防止其他测试的脏数据干扰
	enqueuedFiles = sync.Map{}
	queueUploading = sync.Map{}
	queueSuccess = sync.Map{}
	queueFail = sync.Map{}
	queueRetrying = sync.Map{}
	atomic.StoreInt64(&queueCount, 0)
	atomic.StoreInt64(&queueUploadingCount, 0)
	atomic.StoreInt64(&queueSuccessCount, 0)
	atomic.StoreInt64(&queueFailCount, 0)
	atomic.StoreInt64(&queueRetryingCount, 0)

	// 模拟造数据：5个等待，2个上传中，10个成功，3个失败
	for i := 0; i < 5; i++ {
		enqueuedFiles.Store(i, true)
	}
	for i := 0; i < 2; i++ {
		queueUploading.Store(i, true)
	}
	for i := 0; i < 10; i++ {
		queueSuccess.Store(i, true)
	}
	for i := 0; i < 3; i++ {
		queueFail.Store(i, true)
	}

	// 触发统计算法
	restoreQueueCounts()

	// 校验原子计数器的准确性
	if atomic.LoadInt64(&queueCount) != 5 {
		t.Errorf("等待队列计数错误: 预期 5, 获得 %d", atomic.LoadInt64(&queueCount))
	}
	if atomic.LoadInt64(&queueUploadingCount) != 2 {
		t.Errorf("上传队列计数错误: 预期 2, 获得 %d", atomic.LoadInt64(&queueUploadingCount))
	}
	if atomic.LoadInt64(&queueSuccessCount) != 10 {
		t.Errorf("成功队列计数错误: 预期 10, 获得 %d", atomic.LoadInt64(&queueSuccessCount))
	}
	if atomic.LoadInt64(&queueFailCount) != 3 {
		t.Errorf("失败队列计数错误: 预期 3, 获得 %d", atomic.LoadInt64(&queueFailCount))
	}
	if atomic.LoadInt64(&queueRetryingCount) != 0 {
		t.Errorf("重试队列计数错误: 预期 0, 获得 %d", atomic.LoadInt64(&queueRetryingCount))
	}
}

// TestHandleStatus 测试 /api/v1/status 接口的 JSON 响应格式与数据完整性。
// 使用 httptest.NewRecorder 直接在内存中模拟 HTTP 请求，免去绑定端口带来的网络开销。
func TestHandleStatus(t *testing.T) {
	// 初始化必要的配置项，避免空指针
	appConfigMu.Lock()
	appConfig = Config{
		ScanInterval: 60,
		Workers:      4,
		DayRate:      1024,
		NightRate:    2048,
		Dirs:         []string{"./test_dir"},
	}
	appConfigMu.Unlock()

	sysStatsMu.Lock()
	cachedDiskFree = 1024 * 1024 * 1024 * 50 // 50GB
	cachedFFmpegMem = 1024 * 500             // 500MB
	sysStatsMu.Unlock()

	// 构造 HTTP 请求和响应记录器
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()

	// 直接调用 Handler
	handleStatus(w, req)

	res := w.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("接口响应状态码错误: 预期 200, 获得 %d", res.StatusCode)
	}

	var response APIResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatalf("无法解析 JSON 响应: %v", err)
	}

	if response.Code != 200 {
		t.Errorf("业务状态码错误: 预期 200, 获得 %d", response.Code)
	}

	// 提取 Data 部分进行关键字段断言
	dataMap, ok := response.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("返回的 Data 不是字典格式")
	}

	if int(dataMap["workers"].(float64)) != 4 {
		t.Errorf("Workers 数量映射错误: 预期 4")
	}
	if int(dataMap["scanningInterval"].(float64)) != 60 {
		t.Errorf("ScanInterval 映射错误: 预期 60")
	}
}

// BenchmarkEncryptPayload 极速性能压测：AES-GCM 加密算法执行效率。
// 通过 b.RunParallel 榨干多核 CPU，验证商业级加密层是否会成为高并发下的性能瓶颈。
// 运行方式: go test -bench=BenchmarkEncryptPayload -benchmem
func BenchmarkEncryptPayload(b *testing.B) {
	key := make([]byte, 32)
	rand.Read(key)
	payload := []byte(`{"event":"dashboard_update","cpu_usage":45.2,"mem_usage":1024.5,"active_tasks":12,"network_speed":154200}`)

	b.ResetTimer() // 重置计时器，排除初始化开销

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := encryptPayload(payload, key)
			if err != nil {
				b.Fatalf("加密基准测试崩溃: %v", err)
			}
		}
	})
}

// BenchmarkBuiltinSM3 极速性能压测：内置 SM3 国密散列算法计算效率。
// 验证逆向破解抖音 a_bogus 时的本地 Hash 计算速度能否跟上大规模直播间监控的轮询频率。
// 运行方式: go test -bench=BenchmarkBuiltinSM3 -benchmem
func BenchmarkBuiltinSM3(b *testing.B) {
	testData := "aid=6383&app_name=douyin_web&browser_language=zh-CN&browser_name=Chrome&browser_platform=Win32&browser_version=116.0.0.0&device_platform=web&language=zh-CN&live_id=1&msToken=&web_rid=1234567890cus"

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		// 每个 Goroutine 独立实例化 SM3 计算器避免锁争用
		for pb.Next() {
			sm3 := NewBuiltinSM3()
			sm3.Write(testData)
			_ = sm3.Sum()
		}
	})
}
