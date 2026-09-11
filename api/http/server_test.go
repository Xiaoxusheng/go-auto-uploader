package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"upload/internal/app"
	"upload/internal/config"
	"upload/internal/recorder"
	"upload/internal/uploader"
)

func TestEncryptDecryptPayload(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("无法生成测试密钥: %v", err)
	}
	original := []byte(`{"username":"admin","action":"start_engine"}`)
	enc, err := EncryptPayload(original, key)
	if err != nil {
		t.Fatalf("加密过程发生异常: %v", err)
	}
	if enc == "" {
		t.Fatal("加密结果为空字符串")
	}
	dec, err := DecryptPayload(enc, key)
	if err != nil {
		t.Fatalf("解密过程发生异常: %v", err)
	}
	if !bytes.Equal(original, dec) {
		t.Errorf("解密后的数据与原数据不一致! 预期 %s, 获得 %s", original, dec)
	}
	if _, err := DecryptPayload(enc[:len(enc)-2]+"==", key); err == nil {
		t.Error("安全漏洞：被篡改的密文应当解密失败，但却成功了")
	}
	wrongKey := make([]byte, 32)
	rand.Read(wrongKey)
	if _, err := DecryptPayload(enc, wrongKey); err == nil {
		t.Error("安全漏洞：使用错误的 AES 密钥应当解密失败，但却成功了")
	}
}

func TestRestoreQueueCounts(t *testing.T) {
	app.TaskQueue = uploader.NewQueue(100)
	app.QueueUploading = sync.Map{}
	app.QueueSuccess = sync.Map{}
	app.QueueFail = sync.Map{}
	app.QueueRetrying = sync.Map{}
	atomic.StoreInt64(&app.QueueCount, 0)
	atomic.StoreInt64(&app.QueueUploadingCount, 0)
	atomic.StoreInt64(&app.QueueSuccessCount, 0)
	atomic.StoreInt64(&app.QueueFailCount, 0)
	atomic.StoreInt64(&app.QueueRetryingCount, 0)

	for i := 0; i < 5; i++ {
		app.TaskQueue.Enqueue("/wait/" + string(rune('a'+i)))
	}
	for i := 0; i < 2; i++ {
		app.QueueUploading.Store(i, true)
	}
	for i := 0; i < 10; i++ {
		app.QueueSuccess.Store(i, true)
	}
	for i := 0; i < 3; i++ {
		app.QueueFail.Store(i, true)
	}

	RestoreQueueCounts()

	if atomic.LoadInt64(&app.QueueCount) != 5 {
		t.Errorf("等待队列计数错误: 预期 5, 获得 %d", atomic.LoadInt64(&app.QueueCount))
	}
	if atomic.LoadInt64(&app.QueueUploadingCount) != 2 {
		t.Errorf("上传队列计数错误: 预期 2, 获得 %d", atomic.LoadInt64(&app.QueueUploadingCount))
	}
	if atomic.LoadInt64(&app.QueueSuccessCount) != 10 {
		t.Errorf("成功队列计数错误: 预期 10, 获得 %d", atomic.LoadInt64(&app.QueueSuccessCount))
	}
	if atomic.LoadInt64(&app.QueueFailCount) != 3 {
		t.Errorf("失败队列计数错误: 预期 3, 获得 %d", atomic.LoadInt64(&app.QueueFailCount))
	}
	if atomic.LoadInt64(&app.QueueRetryingCount) != 0 {
		t.Errorf("重试队列计数错误: 预期 0, 获得 %d", atomic.LoadInt64(&app.QueueRetryingCount))
	}
}

func TestHandleStatus(t *testing.T) {
	app.CfgStore.Replace(config.Config{
		ScanInterval: 60,
		Workers:      4,
		DayRate:      1024,
		NightRate:    2048,
		Dirs:         []string{"./test_dir"},
	})
	s := newTestServer()
	s.sysStatsMu.Lock()
	s.cachedDisk = 1024 * 1024 * 1024 * 50
	s.cachedFFMem = 1024 * 500
	s.sysStatsMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("接口响应状态码错误: 预期 200, 获得 %d", res.StatusCode)
	}
	var response apiResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatalf("无法解析 JSON 响应: %v", err)
	}
	if response.Code != 200 {
		t.Errorf("业务状态码错误: 预期 200, 获得 %d", response.Code)
	}
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

func BenchmarkEncryptPayload(b *testing.B) {
	key := make([]byte, 32)
	rand.Read(key)
	payload := []byte(`{"event":"dashboard_update","cpu_usage":45.2,"mem_usage":1024.5,"active_tasks":12,"network_speed":154200}`)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := EncryptPayload(payload, key); err != nil {
				b.Fatalf("加密基准测试崩溃: %v", err)
			}
		}
	})
}

func BenchmarkBuiltinSM3(b *testing.B) {
	testData := "aid=6383&app_name=douyin_web&browser_language=zh-CN&browser_name=Chrome&browser_platform=Win32&browser_version=116.0.0.0&device_platform=web&language=zh-CN&live_id=1&msToken=&web_rid=1234567890cus"
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sm3 := recorder.NewSM3()
			sm3.Write(testData)
			_ = sm3.Sum()
		}
	})
}
