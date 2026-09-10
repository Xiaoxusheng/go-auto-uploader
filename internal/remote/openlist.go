// Package remote 抽象远端对象存储客户端。当前实现 OpenList/AList 的登录与 PUT 上传。
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PutResult 是远端 PUT 的标准化响应。
type PutResult struct {
	Code    int
	Message string
}

// OK 表示 HTTP 层与业务码均成功。
func (r PutResult) OK() bool { return r.Code == http.StatusOK }

// Client 远端存储最小接口；后续可扩 S3/WebDAV 等实现。
type Client interface {
	Login(ctx context.Context) error
	Put(ctx context.Context, remotePath string, body io.Reader, size int64) (*PutResult, error)
	Token() string
}

// OpenListClient 对接 OpenList/AList API。
type OpenListClient struct {
	mu       sync.RWMutex
	baseURL  string
	user     string
	pass     string
	token    string
	httpCli  *http.Client
	endpoint string // 可注入测试用完整 URL 前缀
}

// NewOpenListClient 创建客户端。httpClient 可为 nil（使用默认连接池）。
func NewOpenListClient(baseURL, user, pass string, httpClient *http.Client) *OpenListClient {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 0, // 大文件上传不设总超时，依赖进度与限速
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &OpenListClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		httpCli: httpClient,
	}
}

// SetCredentials 热更新地址与账号（配置变更后调用）。
func (c *OpenListClient) SetCredentials(baseURL, user, pass string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.user = user
	c.pass = pass
	c.mu.Unlock()
}

// Token 返回当前 Bearer Token。
func (c *OpenListClient) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

func (c *OpenListClient) snapshot() (base, user, pass, token string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL, c.user, c.pass, c.token
}

// Login 向 /api/auth/login 换取 Token。
func (c *OpenListClient) Login(ctx context.Context) error {
	base, user, pass, _ := c.snapshot()
	form := url.Values{}
	form.Set("Username", user)
	form.Set("Password", pass)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/auth/login",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	var r struct {
		Code int
		Data struct{ Token string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("login decode: %w", err)
	}
	if r.Code != 200 {
		return fmt.Errorf("login failed with code %d", r.Code)
	}
	c.mu.Lock()
	c.token = r.Data.Token
	c.mu.Unlock()
	return nil
}

// Put 以 HTTP PUT 上传到 OpenList /api/fs/put。
// body 通常为已包装进度/限速的 Reader；size 写入 Content-Length。
func (c *OpenListClient) Put(ctx context.Context, remotePath string, body io.Reader, size int64) (*PutResult, error) {
	base, _, _, token := c.snapshot()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/fs/put", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("File-Path", remotePath)
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", token)
	}

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&r); decodeErr != nil {
		r.Code = resp.StatusCode
		r.Message = "解析远端响应体失败或非标准化 JSON 格式"
	} else if r.Code == 0 {
		r.Code = resp.StatusCode
	}
	return &PutResult{Code: r.Code, Message: r.Message}, nil
}
