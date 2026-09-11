// Package web 打包控制台静态资源。
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html
var IndexHTML string

//go:embed vendor
var vendorFS embed.FS

// VendorHandler 返回 /vendor/ 下的前端依赖静态资源处理器。
//
// 依赖全部随二进制发布（vue / arco / axios / echarts），不再走公网 CDN：
// unpkg 未锁版本时每次加载都要先 302 跳到版本化地址，实测仅重定向就需
// 15~20 秒，控制台首屏会长时间空白；自托管后局域网/离线环境也能秒开。
func VendorHandler() http.Handler {
	sub, err := fs.Sub(vendorFS, "vendor")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.StripPrefix("/vendor/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 资源内容随二进制版本固定，可放心长缓存
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}
