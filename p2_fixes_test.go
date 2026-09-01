package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- P2-14 原子写与坏文件隔离 ----

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := writeFileAtomic(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"a":1}` {
		t.Fatalf("content = %q (err=%v), want %q", data, err, `{"a":1}`)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover should not exist after rename")
	}

	// 覆盖写同样成功
	if err := writeFileAtomic(path, []byte(`{"a":2}`), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != `{"a":2}` {
		t.Fatalf("content after overwrite = %q", data)
	}
}

func TestQuarantineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	quarantineFile(path, os.ErrInvalid)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original file should be renamed away")
	}
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatalf(".bad quarantine file missing: %v", err)
	}
}

func TestZenConfigQuarantine(t *testing.T) {
	oldPath, oldCfg := zenConfigPath, zenConfig
	t.Cleanup(func() { zenConfigPath, zenConfig = oldPath, oldCfg })

	dir := t.TempDir()
	zenConfigPath = filepath.Join(dir, ".cline-zen.json")
	if err := os.WriteFile(zenConfigPath, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	zenConfigMu.Lock()
	zenConfig = nil
	zenConfigMu.Unlock()

	cfg := getZenConfig()
	if _, err := os.Stat(zenConfigPath + ".bad"); err != nil {
		t.Fatalf("corrupt zen config not quarantined: %v", err)
	}
	if cfg.Key != "public" || cfg.MaxConcurrency != 8 {
		t.Fatalf("corrupt zen config should fall back to defaults, got %+v", cfg)
	}
}

func TestProxyConfigQuarantine(t *testing.T) {
	oldFile := proxyConfigFile
	t.Cleanup(func() { proxyConfigFile = oldFile })

	dir := t.TempDir()
	proxyConfigFile = filepath.Join(dir, ".cline-config.json")
	if err := os.WriteFile(proxyConfigFile, []byte("[[[nope"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := loadProxyConfigFromDisk()
	if _, err := os.Stat(proxyConfigFile + ".bad"); err != nil {
		t.Fatalf("corrupt proxy config not quarantined: %v", err)
	}
	if cfg.Strategy != "round_robin" {
		t.Fatalf("corrupt proxy config should fall back to default strategy, got %q", cfg.Strategy)
	}
}

// ---- P2-6 token 刷新：鉴权拒绝与暂态失败区分 ----

func TestRefreshClineTokenTypedErrors(t *testing.T) {
	oldTransport := authClient.Transport
	t.Cleanup(func() { authClient.Transport = oldTransport })

	// 403：上游明确拒绝 → tokenRefreshRejectedError
	authClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"error":"denied"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	_, err := refreshClineToken("rt")
	var rej *tokenRefreshRejectedError
	if !errors.As(err, &rej) || rej.status != http.StatusForbidden {
		t.Fatalf("403 should yield tokenRefreshRejectedError, got %v (%T)", err, err)
	}

	authClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	_, err = refreshClineToken("rt")
	var rej2 *tokenRefreshRejectedError
	if errors.As(err, &rej2) {
		t.Fatalf("500 must NOT be tokenRefreshRejectedError, got %v", err)
	}
	if err == nil {
		t.Fatal("500 should return an error")
	}

	authClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	_, err = refreshClineToken("rt")
	if errors.As(err, &rej2) {
		t.Fatalf("transport error must NOT be tokenRefreshRejectedError, got %v", err)
	}
}

func TestRefreshTransportErrorKeepsAccountActive(t *testing.T) {
	oldPool, oldTransport := pool, authClient.Transport
	t.Cleanup(func() { pool, authClient.Transport = oldPool, oldTransport })

	acc := &Account{
		AccountID:    "transient",
		Email:        "transient@example.com",
		RefreshToken: "rt",
		Status:       "active",
	}
	pool = &AccountPool{Accounts: []*Account{acc}}

	authClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded // 模拟网络抖动
	})

	err := refreshAccountToken(acc)
	if err == nil {
		t.Fatal("expected error on transport failure")
	}
	if acc.Status != "active" {
		t.Fatalf("transient refresh failure must NOT expire account, got %q", acc.Status)
	}
}

func TestRefreshRejectedMarksExpired(t *testing.T) {
	oldPool, oldTransport := pool, authClient.Transport
	t.Cleanup(func() { pool, authClient.Transport = oldPool, oldTransport })

	acc := &Account{
		AccountID:    "revoked",
		Email:        "revoked@example.com",
		RefreshToken: "rt",
		Status:       "active",
	}
	pool = &AccountPool{Accounts: []*Account{acc}}

	authClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	err := refreshAccountToken(acc)
	if err == nil {
		t.Fatal("expected error on 401 rejection")
	}
	if acc.Status != "expired" {
		t.Fatalf("4xx rejection should expire account, got %q", acc.Status)
	}
}

// ---- P2-9 上游状态码透传与 Retry-After ----

func TestUpstreamErrorHTTPStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{&clineAPIError{statusCode: 403, message: "forbidden"}, 403},
		{&clineAPIError{statusCode: 429, message: "rate"}, 429},
		{&clineAPIError{statusCode: 502, message: "bad gw"}, 502},
		{&freeModelUnavailableError{message: "no eligible accounts"}, 429},
		{&zenAPIError{statusCode: 429, message: "rate"}, 429},
		{&zenAPIError{statusCode: 403, message: "forbidden"}, 403},
		{&zenAPIError{statusCode: 503, message: "unavailable"}, 503},
		{&zenAPIError{statusCode: 0, message: "unknown"}, 502},
		{fmt.Errorf("no active accounts available"), 500},
		{&clineAccountUnavailableError{err: fmt.Errorf("transport down")}, 500},
	}
	for _, c := range cases {
		if got := upstreamErrorHTTPStatus(c.err); got != c.want {
			t.Errorf("upstreamErrorHTTPStatus(%T) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestWriteUpstreamErrorRetryAfter(t *testing.T) {
	// zen 429 带 Retry-After → 回填秒数
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, &zenAPIError{statusCode: 429, message: "slow down", retryAfter: 45 * time.Second})
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("zen 429 with Retry-After should set the header")
	}

	// zen 429 无 Retry-After → 不设头
	rec = httptest.NewRecorder()
	writeUpstreamError(rec, &zenAPIError{statusCode: 429, message: "slow down"})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("zen 429 without Retry-After: status=%d header=%q", rec.Code, rec.Header().Get("Retry-After"))
	}

	// cline 429 错误文本含 "Try again in 45m" → 回填（P1-8 修复后的分钟级解析）
	rec = httptest.NewRecorder()
	writeUpstreamError(rec, &clineAPIError{statusCode: 429, message: "Try again in 45m"})
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 40*60 || ra > 46*60 {
		t.Fatalf("Retry-After = %q, want ≈45m in seconds", rec.Header().Get("Retry-After"))
	}

	// 非 429 → 不设头
	rec = httptest.NewRecorder()
	writeUpstreamError(rec, &clineAPIError{statusCode: 403, message: "Try again in 45m"})
	if rec.Code != 403 || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("403 should not carry Retry-After: status=%d header=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// 端到端：上游 403 必须原样到达客户端（修复前坍缩为 500）。
func TestChatPropagatesUpstreamStatus(t *testing.T) {
	oldPool := pool
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		httpClient.Transport = oldTransport
	})

	baseURL := protocolTestServer(t)

	acc := &Account{
		AccountID:   "prop1",
		Email:       "prop@example.com",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{acc}, Keys: []string{}, Models: []Model{}}

	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"error":"account suspended"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"some-nonexistent-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 403 (body=%s)", resp.StatusCode, body)
	}
}