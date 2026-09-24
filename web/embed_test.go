package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// vendor 资源必须齐全且可正常返回，否则控制台会白屏。
func TestVendorHandlerServesAssets(t *testing.T) {
	h := VendorHandler()
	assets := []string{
		"vue.global.prod.js",
		"arco-vue.min.js",
		"arco-vue-icon.min.js",
		"axios.min.js",
		"echarts.min.js",
		"arco.min.css",
	}
	for _, name := range assets {
		req := httptest.NewRequest(http.MethodGet, "/vendor/"+name, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", name, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: 响应体为空", name)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Fatalf("%s: 缺少长缓存头, got %q", name, cc)
		}
	}
}

// index.html 不得残留公网 CDN 引用：unpkg 未锁版本时会先 302 跳转，
// 实测仅重定向就需 15~20 秒，且局域网/离线环境直接白屏。
func TestIndexHasNoPublicCDN(t *testing.T) {
	for _, bad := range []string{"unpkg.com", "jsdelivr.net", "cdnjs.cloudflare.com"} {
		if strings.Contains(IndexHTML, bad) {
			t.Fatalf("index.html 仍引用公网 CDN: %s", bad)
		}
	}
	for _, ref := range []string{
		"/vendor/vue.global.prod.js",
		"/vendor/axios.min.js",
		"/vendor/echarts.min.js",
	} {
		if !strings.Contains(IndexHTML, ref) {
			t.Fatalf("index.html 缺少自托管引用: %s", ref)
		}
	}
	// 重设计后前端为纯手写组件，不再加载 Arco（避免 ~800KB 无用解析开销）
	for _, stale := range []string{"arco-vue.min.js", "arco.min.css", "ArcoVue."} {
		if strings.Contains(IndexHTML, stale) {
			t.Fatalf("index.html 不应再引用 Arco: %s", stale)
		}
	}
}

// 移动端必须保留双指缩放能力（禁用 user-scalable 属无障碍问题）。
func TestViewportAllowsZoom(t *testing.T) {
	if strings.Contains(IndexHTML, "user-scalable=no") || strings.Contains(IndexHTML, "maximum-scale=1") {
		t.Fatal("viewport 不应禁用缩放")
	}
}
