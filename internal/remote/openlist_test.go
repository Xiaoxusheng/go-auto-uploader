package remote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenListLoginAndPut(t *testing.T) {
	t.Parallel()
	var putAuth, putPath string
	var putLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = r.ParseForm()
			if r.Form.Get("Username") != "admin" || r.Form.Get("Password") != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"token":"tok-123"}}`))
		case r.URL.Path == "/api/fs/put":
			putAuth = r.Header.Get("Authorization")
			putPath = r.Header.Get("File-Path")
			putLen = r.ContentLength
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewOpenListClient(srv.URL, "admin", "secret", srv.Client())
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Token() != "tok-123" {
		t.Fatalf("token=%q", c.Token())
	}

	body := strings.NewReader("hello")
	res, err := c.Put(ctx, "/dest/a.ts", body, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("put result %+v", res)
	}
	if putAuth != "tok-123" || putPath != "/dest/a.ts" || putLen != 5 {
		t.Fatalf("put meta auth=%q path=%q len=%d", putAuth, putPath, putLen)
	}
}

func TestOpenListLoginFail(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":401}`))
	}))
	defer srv.Close()
	c := NewOpenListClient(srv.URL, "u", "p", srv.Client())
	if err := c.Login(context.Background()); err == nil {
		t.Fatal("expected login error")
	}
}
