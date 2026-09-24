package recorder

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

func apiProxyImage(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}

	// 安全审计修复：仅允许 http/https 协议，阻断 file/gopher 等危险协议
	u, err := url.Parse(targetURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "仅允许 http/https 的公开图片地址", http.StatusBadRequest)
		return
	}
	targetURL = u.String()

	doProxy := func(withReferer bool) (*http.Response, error) {
		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Cache-Control", "no-cache")

		if withReferer {
			if strings.Contains(targetURL, "douyinpic.com") || strings.Contains(targetURL, "douyincdn.com") || strings.Contains(targetURL, "byteimg.com") {
				req.Header.Set("Referer", "https://live.douyin.com/")
			} else if strings.Contains(targetURL, "kuaishou") || strings.Contains(targetURL, "yximgs.com") {
				req.Header.Set("Referer", "https://live.kuaishou.com/")
			}
		}

		// 安全审计修复：改用带内网 IP 校验的专用客户端，阻断对 127.0.0.1/内网段/云元数据的探测
		return builtinSafeProxyClient.Do(req)
	}

	resp, err := doProxy(true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if resp.StatusCode == 403 || resp.StatusCode == 401 {
		resp.Body.Close()
		resp, err = doProxy(false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		return
	}

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	// 防缓存投毒：上游（平台图床）可能对失效/风控请求返回 200+空体且带超长 max-age，
	// 若原样透传，浏览器会把空图缓存一年，之后即使网络恢复也永远显示占位图。
	// 这里强制覆盖为短缓存，过期后自动重新拉取（真图/新签名 URL 均能自愈）。
	w.Header().Set("Cache-Control", "max-age=60")
	w.Header().Del("Expires")
	// 安全审计修复：限制代理回源体积，防止超大响应拖垮进程内存
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 10<<20))
}

// ==========================================
// 🌟 终极截帧黑科技：内存级多路分发截帧
// ==========================================

// builtinTailBuffer 环形日志尾部缓冲池，避免收集底层日志时消耗过多内存
