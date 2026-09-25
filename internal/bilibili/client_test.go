package bilibili

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newMockUpoos 起一个假的 upos 网关：init(POST) / 分片(PUT) / 合片(POST submit=add)。
// 收到的分片字节按 chunk 序号拼回 parts，供断言「上传字节 == 原文件字节」。
func newMockUpoos(t *testing.T) (srv *httptest.Server, parts *map[int][]byte) {
	t.Helper()
	store := map[int][]byte{}
	var mu sync.Mutex
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if got := r.Header.Get("X-Upos-Auth"); got != "AK-RAW-STRING" {
			t.Errorf("upos X-Upos-Auth = %q, 期望原始 auth 字符串", got)
		}
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			if q.Get("profile") != "ugcfx/bup" {
				t.Errorf("init profile = %q", q.Get("profile"))
			}
			if q.Get("filesize") == "" || q.Get("partsize") == "" || q.Get("biz_id") == "" {
				t.Errorf("init 缺少 filesize/partsize/biz_id: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"OK":1,"upload_id":"UPID-1","bucket":"b","key":"k"}`))
		case r.Method == http.MethodPut && q.Get("partNumber") != "":
			n := 0
			fmt.Sscanf(q.Get("partNumber"), "%d", &n)
			buf := make([]byte, r.ContentLength)
			read, _ := io.ReadFull(r.Body, buf)
			store[n] = buf[:read]
			if q.Get("end") == "" || q.Get("total") == "" || q.Get("chunk") == "" {
				t.Errorf("分片缺少 end/total/chunk: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`MULTIPART_PUT_SUCCESS`))
		case r.Method == http.MethodPost && q.Get("submit") == "add":
			var body struct {
				Parts []map[string]interface{} `json:"parts"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("合片请求体解析失败: %v", err)
			}
			if len(body.Parts) == 0 {
				t.Errorf("合片 parts 为空")
			}
			for i, p := range body.Parts {
				if p["partNumber"].(float64) != float64(i+1) {
					t.Errorf("合片 partNumber[%d] = %v", i, p["partNumber"])
				}
			}
			w.Write([]byte(`{"OK":1,"location":"ugc/m.mp4","bucket":"b","key":"/m.mp4"}`))
		default:
			t.Errorf("unexpected upos request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &store
}

// newMockMember 起一个假的 member.bilibili.com。新版 upos_uri 不含主机名，
// 上传地址由 endpoint 给出（本地 mock 用 http 线路），auth 为原始字符串。
func newMockMember(t *testing.T, upos *httptest.Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/preupload":
			q := r.URL.Query()
			if q.Get("r") != "upos" || q.Get("profile") != "ugcfx/bup" {
				t.Errorf("preupload r/profile = %q/%q", q.Get("r"), q.Get("profile"))
			}
			host := strings.TrimPrefix(upos.URL, "http://")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"OK":        1,
				"auth":      "AK-RAW-STRING",
				"biz_id":    4242,
				"chunk_size": 4 << 20,
				"endpoint":  "http://" + host,
				"upos_uri":  "upos://ugc-zone/mockname.mp4",
			})
		case "/x/vu/web/cover/up":
			if err := r.ParseForm(); err != nil {
				t.Errorf("cover/up ParseForm: %v", err)
			}
			cover := r.FormValue("cover")
			if !strings.HasPrefix(cover, "data:image/jpeg;base64,") {
				t.Errorf("cover data URI 前缀异常: %.40s", cover)
			}
			w.Write([]byte(`{"code":0,"message":"0","data":{"url":"http://i0.hdslb.com/bfs/archive/mock.jpg"}}`))
		case "/x/vu/web/add/v3":
			raw, _ := io.ReadAll(r.Body)
			t.Logf("add/v3 payload: %s", raw)
			if r.URL.Query().Get("csrf") == "" {
				t.Errorf("add/v3 缺少 csrf")
			}
			if !strings.Contains(r.Header.Get("Cookie"), "SESSDATA=") {
				t.Errorf("add/v3 缺少 SESSDATA cookie")
			}
			w.Write([]byte(`{"code":0,"message":"0","data":{"aid":9527,"bvid":"BV1MOCK"}}`))
		default:
			t.Errorf("unexpected member request: %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, member string) *Client {
	t.Helper()
	c := NewClient("SESS-ABC", "JCT-XYZ", "12345")
	c.memberBase = member
	c.apiBase = member
	return c
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUploadVideoEndToEnd 一次完整上传：字节级校验 + 返回文件名与 biz_id。
func TestUploadVideoEndToEnd(t *testing.T) {
	upos, parts := newMockUpoos(t)
	member := newMockMember(t, upos)
	c := newTestClient(t, member.URL)

	// 10MB + 1 字节：4MiB 分片 → 3 片，验证非整分片边界
	data := make([]byte, 10<<20+1)
	for i := range data {
		data[i] = byte(i * 7)
	}
	src := writeTempFile(t, t.TempDir(), "src.mp4", data)

	var progressCalls int
	res, err := c.UploadVideo(context.Background(), src, func(done, total int64) {
		progressCalls++
		if done > total {
			t.Errorf("progress done=%d > total=%d", done, total)
		}
	})
	if err != nil {
		t.Fatalf("UploadVideo: %v", err)
	}
	if res.Filename != "mockname" {
		t.Errorf("Filename = %q, 期望 upos_uri 基名去扩展名", res.Filename)
	}
	if res.BizID != 4242 {
		t.Errorf("BizID = %d", res.BizID)
	}
	if progressCalls != 3 {
		t.Errorf("progress 回调次数 = %d, 期望 3", progressCalls)
	}

	// 分片字节拼回必须与原文件完全一致
	got := []byte{}
	for i := 1; ; i++ {
		p, ok := (*parts)[i]
		if !ok {
			break
		}
		got = append(got, p...)
	}
	if len(got) != len(data) {
		t.Fatalf("收到的字节数 = %d, 期望 %d", len(got), len(data))
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("字节不一致 @ %d", i)
		}
	}
}

// TestUploadVideoPreuploadReject 未过预上传时整体失败。
func TestUploadVideoPreuploadReject(t *testing.T) {
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"OK":0,"msg":"risk control"}`))
	}))
	defer member.Close()
	c := newTestClient(t, member.URL)

	src := writeTempFile(t, t.TempDir(), "x.mp4", []byte("hello"))
	if _, err := c.UploadVideo(context.Background(), src, nil); err == nil {
		t.Fatal("期望预上传失败报错")
	}
}

// TestSubmitArchive 提交稿件：字段映射、cid=biz_id、标题截断。
func TestSubmitArchive(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/x/vu/web/add/v3" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"code":0,"message":"0","data":{"aid":9527,"bvid":"BV1MOCK"}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	longTitle := strings.Repeat("舞", 100)
	aid, bvid, err := c.SubmitArchive(context.Background(), ArchiveParams{
		Title:         longTitle,
		Desc:          "测试简介",
		Tag:           "直播,高光",
		Tid:           129,
		Copyright:     1,
		NoReprint:     true,
		BizID:         4242,
		VideoFilename: "mockname.mp4",
	})
	if err != nil {
		t.Fatalf("SubmitArchive: %v", err)
	}
	if aid != 9527 || bvid != "BV1MOCK" {
		t.Errorf("aid/bvid = %d/%s", aid, bvid)
	}
	if got["copyright"].(float64) != 1 || got["tid"].(float64) != 129 {
		t.Errorf("copyright/tid = %v/%v", got["copyright"], got["tid"])
	}
	if got["tag"] != "直播,高光" {
		t.Errorf("tag = %v", got["tag"])
	}
	if noReprint, _ := got["no_reprint"].(float64); noReprint != 1 {
		t.Errorf("no_reprint = %v", got["no_reprint"])
	}
	if got["web_os"].(float64) != 3 || got["recreate"].(float64) != -1 {
		t.Errorf("2024 版必填字段缺失: web_os/recreate")
	}
	videos := got["videos"].([]interface{})
	v0 := videos[0].(map[string]interface{})
	if v0["filename"] != "mockname" {
		t.Errorf("filename = %v, 期望去掉扩展名", v0["filename"])
	}
	if v0["cid"].(float64) != 4242 {
		t.Errorf("cid = %v, 期望 = biz_id", v0["cid"])
	}
	if title, _ := got["title"].(string); len([]rune(title)) != 80 {
		t.Errorf("title rune 数 = %d, 期望截断到 80", len([]rune(title)))
	}
}

// TestSubmitArchiveFallback21150 add/v3 返回 21150 时回退老接口 x/vu/web/add。
func TestSubmitArchiveFallback21150(t *testing.T) {
	v3Hits, addHits, geetestHits := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/x/geetest/pre/add":
			geetestHits++
			w.Write([]byte(`{"code":0}`))
		case "/x/vu/web/add/v3":
			v3Hits++
			w.Write([]byte(`{"code":21150,"message":"投稿入口升级中，请重新编辑稿件"}`))
		case "/x/vu/web/add":
			addHits++
			w.Write([]byte(`{"code":0,"message":"0","data":{"aid":1,"bvid":"BV1FB"}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	aid, bvid, err := c.SubmitArchive(context.Background(), ArchiveParams{Tid: 174, Copyright: 1, VideoFilename: "a.mp4"})
	if err != nil {
		t.Fatalf("SubmitArchive: %v", err)
	}
	if aid != 1 || bvid != "BV1FB" {
		t.Errorf("aid/bvid = %d/%s", aid, bvid)
	}
	if v3Hits != 1 || addHits != 1 || geetestHits != 1 {
		t.Errorf("调用次数 v3/add/geetest = %d/%d/%d", v3Hits, addHits, geetestHits)
	}
}

// TestAPIErrorOnBusinessCode 业务码非 0 转为 APIError 并保留 code。
func TestAPIErrorOnBusinessCode(t *testing.T) {	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":601,"message":"投稿太频繁"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	_, _, err := c.SubmitArchive(context.Background(), ArchiveParams{Tid: 1, Copyright: 1, VideoFilename: "a.mp4"})
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("期望 *APIError, got %T: %v", err, err)
	}
	if ae.Code != 601 {
		t.Errorf("code = %d", ae.Code)
	}
}

// TestNav 登录态校验：有效 / 失效。
func TestNav(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "bili_jct=JCT-XYZ") {
			t.Errorf("nav cookie 异常: %q", r.Header.Get("Cookie"))
		}
		w.Write([]byte(`{"code":0,"message":"0","data":{"isLogin":true,"mid":42,"uname":"测试君","level":6}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	info, err := c.Nav(context.Background())
	if err != nil {
		t.Fatalf("Nav: %v", err)
	}
	if !info.IsLogin || info.Mid != 42 || info.Uname != "测试君" {
		t.Errorf("info = %+v", info)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":-101,"message":"账号未登录"}`))
	}))
	defer dead.Close()
	c2 := newTestClient(t, dead.URL)
	if _, err := c2.Nav(context.Background()); err == nil {
		t.Fatal("期望登录失效报错")
	}
}

// TestUploadCover 封面上传：data URI 与 base64 正确、返回 URL。
func TestUploadCover(t *testing.T) {
	upos, _ := newMockUpoos(t)
	member := newMockMember(t, upos)

	var coverB64 string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		cover := r.FormValue("cover")
		coverB64 = strings.TrimPrefix(cover, "data:image/jpeg;base64,")
		if r.FormValue("csrf") != "JCT-XYZ" {
			t.Errorf("csrf = %q", r.FormValue("csrf"))
		}
		w.Write([]byte(`{"code":0,"data":{"url":"http://i0.hdslb.com/x.jpg"}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.apiBase = member.URL

	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	p := writeTempFile(t, t.TempDir(), "cover.jpg", jpg)
	u, err := c.UploadCover(context.Background(), p)
	if err != nil {
		t.Fatalf("UploadCover: %v", err)
	}
	if u != "http://i0.hdslb.com/x.jpg" {
		t.Errorf("url = %q", u)
	}
	decoded, err := base64.StdEncoding.DecodeString(coverB64)
	if err != nil || string(decoded) != string(jpg) {
		t.Errorf("base64 内容不一致: %v", err)
	}
}

// TestUposTarget 目标解析：endpoint 优先，兼容旧形态。
func TestUposTarget(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		uri        string
		wantBase   string
		wantPath   string
		wantErr    bool
	}{
		{"新版 endpoint+bucket uri", "//upos-cs-upcdntxa.bilivideo.com", "upos://ugcfx2lf/f.mp4", "https://upos-cs-upcdntxa.bilivideo.com", "/ugcfx2lf/f.mp4", false},
		{"endpoint http 本地", "http://127.0.0.1:1", "upos://b/f.mp4", "http://127.0.0.1:1", "/b/f.mp4", false},
		{"旧形态 uri 自带主机", "", "upos://old-host.com/zone/f.mp4", "https://old-host.com", "/zone/f.mp4", false},
		{"全空", "", "", "", "", true},
		{"uri 无路径", "", "upos://noSlash", "", "", true},
	}
	for _, tc := range cases {
		pre := preuploadResp{Endpoint: tc.endpoint, UposURI: tc.uri, UploadURL: tc.endpoint}
		b, p, err := uposTarget(pre)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: 期望报错", tc.name)
			}
			continue
		}
		if err != nil || b != tc.wantBase || p != tc.wantPath {
			t.Errorf("%s: %s/%s/%v, 期望 %s/%s", tc.name, b, p, err, tc.wantBase, tc.wantPath)
		}
	}
}

// TestVideoName 文件名提取。
func TestVideoName(t *testing.T) {
	pre := preuploadResp{UposURI: "upos://ugcfx2lf/n240729xyz.mkv"}
	if pre.VideoName() != "n240729xyz" {
		t.Errorf("VideoName = %q", pre.VideoName())
	}
	pre2 := preuploadResp{BiliFilename: "old.mp4"}
	if pre2.VideoName() != "old" {
		t.Errorf("VideoName(旧) = %q", pre2.VideoName())
	}
}

// TestUposAuthString 兼容原始字符串与旧 JSON 两种 auth。
func TestUposAuthString(t *testing.T) {
	if s := (preuploadResp{Auth: "ak=1&os=upos"}).uposAuthString(); s != "ak=1&os=upos" {
		t.Errorf("raw = %q", s)
	}
	if s := (preuploadResp{Auth: `{"upcdn":"bda2","token":"TOK"}`}).uposAuthString(); s != "TOK" {
		t.Errorf("json token = %q", s)
	}
}

// TestCookieHeader 空字段跳过、字段齐全。
func TestCookieHeader(t *testing.T) {
	full := NewClient("S", "J", "D").cookieHeader()
	if full != "bili_jct=J; SESSDATA=S; DedeUserID=D" {
		t.Errorf("full = %q", full)
	}
	partial := NewClient("S", "", "").cookieHeader()
	if partial != "SESSDATA=S" {
		t.Errorf("partial = %q", partial)
	}
}
