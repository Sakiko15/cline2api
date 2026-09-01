package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

// ---- P2-10 非流式 Anthropic：文本与工具块共存 ----

func TestOpenAIToAnthropicKeepsTextWithToolCalls(t *testing.T) {
	upstream := map[string]any{
		"model": "m",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": "Let me check that for you.",
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "fs_read",
						"arguments": `{"path":"a.txt"}`,
					},
				}},
			},
		}},
	}
	out := openAIToAnthropic(upstream)

	blocks, _ := out["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content should keep text + tool_use, got %v", blocks)
	}
	if blocks[0].(map[string]any)["type"] != "text" || blocks[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("block order wrong: %v", blocks)
	}
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", out["stop_reason"])
	}
}

// 上游报 stop/length 但带 tool_calls 时的 stop_reason 映射。
func TestOpenAIToAnthropicStopReasonMapping(t *testing.T) {
	mk := func(finish string) map[string]any {
		return map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": finish,
				"message": map[string]any{
					"content":    "x",
					"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}},
				},
			}},
		}
	}
	if out := openAIToAnthropic(mk("stop")); out["stop_reason"] != "tool_use" {
		t.Errorf("finish=stop + tool_calls → %v, want tool_use", out["stop_reason"])
	}
	if out := openAIToAnthropic(mk("length")); out["stop_reason"] != "max_tokens" {
		t.Errorf("finish=length + tool_calls → %v, want max_tokens", out["stop_reason"])
	}
	plain := map[string]any{
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": "plain"}}},
	}
	if out := openAIToAnthropic(plain); out["stop_reason"] != "end_turn" {
		t.Errorf("finish=stop without tools → %v, want end_turn", out["stop_reason"])
	}
}

// ---- P2-12 流式错误终止 ----

// Responses 流：上游 error 事件 → response.failed，不再伪造成 completed。
func TestResponsesStreamErrorEmitsFailed(t *testing.T) {
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"message\":\"boom\",\"type\":\"upstream\"}}\n\n")),
		Header: make(http.Header),
	}
	rec := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: "m"}

	chatStreamToResponses(rec, upstream, reqLog, nil)

	out := rec.Body.String()
	if !strings.Contains(out, "response.failed") {
		t.Fatalf("should emit response.failed, got %s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("must not emit response.completed after upstream error, got %s", out)
	}
	if reqLog.Completed {
		t.Fatal("request log must be finalized as failed")
	}
}

// Anthropic 流：上游 error 事件后不再补发 message_delta/message_stop，日志记 failed。
func TestAnthropicStreamErrorSkipsEpilogue(t *testing.T) {
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"message\":\"boom\",\"type\":\"upstream\"}}\n\n")),
		Header: make(http.Header),
	}
	rec := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "m"}

	acc := &Account{AccountID: "a1", Email: "a@example.com", Status: "active"}
	handleAnthropicStream(rec, upstream, acc, reqLog)

	out := rec.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("should emit error event, got %s", out)
	}
	if strings.Contains(out, "message_delta") || strings.Contains(out, "message_stop") {
		t.Fatalf("must not send message_delta/message_stop after upstream error, got %s", out)
	}
	if reqLog.Completed {
		t.Fatal("request log must be finalized as failed")
	}
}
// ---- P2-2 CSRF 同源校验 ----

func TestAdminSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		host    string
		want    bool
	}{
		{"sec-fetch same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, "localhost:3457", true},
		{"sec-fetch none", map[string]string{"Sec-Fetch-Site": "none"}, "localhost:3457", true},
		{"sec-fetch cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, "localhost:3457", false},
		{"sec-fetch same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, "localhost:3457", false},
		{"origin matches", map[string]string{"Origin": "http://localhost:3457"}, "localhost:3457", true},
		{"origin mismatch", map[string]string{"Origin": "http://evil.example"}, "localhost:3457", false},
		{"origin null", map[string]string{"Origin": "null"}, "localhost:3457", false},
		{"referer matches", map[string]string{"Referer": "http://localhost:3457/admin/"}, "localhost:3457", true},
		{"forwarded host matches", map[string]string{"Origin": "https://api.example.com", "X-Forwarded-Host": "api.example.com"}, "internal:3457", true},
		{"no headers allows non-browser", nil, "localhost:3457", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "http://"+c.host+"/admin/api/login", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := adminSameOrigin(r); got != c.want {
			t.Errorf("%s: adminSameOrigin = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---- P2-3 key 强随机与鉴权 ----

func TestAdminGenerateKeyIsRandom(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}

	keys := map[string]bool{}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handleAdminGenerateKey(rec, httptest.NewRequest("POST", "/admin/api/keys/generate", nil))
		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Key string `json:"key"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
		}
		key := resp.Data.Key
		if !strings.HasPrefix(key, "cline_") {
			t.Fatalf("key %q should carry cline_ prefix", key)
		}
		if len(key) != len("cline_")+64 {
			t.Fatalf("key %q should be cline_ + 64 hex chars", key)
		}
		if keys[key] {
			t.Fatalf("generated keys must be unique, got duplicate %q", key)
		}
		keys[key] = true
	}
}

// 端到端：存量 key 走常数时间比较后仍能通过鉴权；错误/缺失 key 仍 401。
func TestAPIKeyAuthAcceptsStoredKey(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })

	baseURL := protocolTestServer(t)
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{"cline_secret0123456789abcdef"}, Models: []Model{}}

	get := func(apiKey string) int {
		req, _ := http.NewRequest("GET", baseURL+"/v1/models", nil)
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := get("cline_secret0123456789abcdef"); code != http.StatusOK {
		t.Fatalf("stored key should pass auth and list models, got %d", code)
	}
	if code := get("cline_wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong key should be 401, got %d", code)
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("missing key should be 401, got %d", code)
	}
}

// ---- P2-4 Secure cookie ----

func TestRequestIsHTTPS(t *testing.T) {
	r := httptest.NewRequest("GET", "http://x/", nil)
	if requestIsHTTPS(r) {
		t.Error("plain http request must not be treated as HTTPS")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !requestIsHTTPS(r) {
		t.Error("X-Forwarded-Proto: https should be honored")
	}
	r.Header.Set("X-Forwarded-Proto", "HTTPS")
	if !requestIsHTTPS(r) {
		t.Error("X-Forwarded-Proto comparison should be case-insensitive")
	}
	direct := httptest.NewRequest("GET", "http://x/", nil)
	direct.TLS = &tls.ConnectionState{}
	if !requestIsHTTPS(direct) {
		t.Error("r.TLS != nil should be treated as HTTPS")
	}
}

func TestAdminLoginCookieSecureFlag(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}

	mkReq := func(xfp string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost:3457/admin/api/login",
			strings.NewReader(`{"password":"pw"}`))
		r.Header.Set("Content-Type", "application/json")
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		rec := httptest.NewRecorder()
		handleAdminLogin(rec, r)
		return rec
	}

	if rec := mkReq("https"); !strings.Contains(rec.Header().Get("Set-Cookie"), "Secure") {
		t.Fatalf("HTTPS login should set Secure cookie, got %q", rec.Header().Get("Set-Cookie"))
	}
	if rec := mkReq(""); strings.Contains(rec.Header().Get("Set-Cookie"), "Secure") {
		t.Fatalf("plain HTTP login must not set Secure cookie, got %q", rec.Header().Get("Set-Cookie"))
	}
}

// ---- P2-7 OAuth 会话：懒清扫 + 快照读 ----

func TestOAuthSessionEviction(t *testing.T) {
	old := oauthSessions
	t.Cleanup(func() { oauthSessions = old })
	oauthSessions = map[string]*oauthSessionState{}
	oauthSessions["stale"] = &oauthSessionState{CreatedAt: time.Now().Add(-oauthSessionTTL - time.Minute)}
	oauthSessions["fresh"] = &oauthSessionState{CreatedAt: time.Now()}

	oauthSessionsMu.Lock()
	evictExpiredOAuthSessionsLocked()
	oauthSessionsMu.Unlock()

	if _, ok := oauthSessions["stale"]; ok {
		t.Fatal("expired session should be evicted")
	}
	if _, ok := oauthSessions["fresh"]; !ok {
		t.Fatal("fresh session should survive")
	}
}

func TestOAuthStatusSnapshotRead(t *testing.T) {
	old := oauthSessions
	t.Cleanup(func() { oauthSessions = old })
	oauthSessions = map[string]*oauthSessionState{}
	oauthSessions["s1"] = &oauthSessionState{Done: true, Success: true, Email: "a@b.c", CreatedAt: time.Now()}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/admin/api/oauth/status?sessionId=s1", nil)
	handleOAuthStatus(rec, r)

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Done    bool   `json:"done"`
			Success bool   `json:"success"`
			Email   string `json:"email"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if !resp.Data.Done || !resp.Data.Success || resp.Data.Email != "a@b.c" {
		t.Fatalf("status snapshot mismatch: %+v", resp.Data)
	}
}

// ---- P2-16 上游头覆盖校验 ----

func TestAdminHeaderOverrideValidation(t *testing.T) {
	oldFile, oldCfg := proxyConfigFile, getProxyConfig()
	t.Cleanup(func() {
		proxyConfigMu.Lock()
		proxyConfig = oldCfg // 仅恢复内存态，不回写磁盘
		proxyConfigMu.Unlock()
		proxyConfigFile = oldFile
	})
	proxyConfigFile = filepath.Join(t.TempDir(), ".cline-config.json")

	post := func(headers map[string]string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{"headers": headers})
		r := httptest.NewRequest("POST", "/admin/api/config/update", bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handleAdminUpdateConfig(rec, r)
		return rec
	}

	if rec := post(map[string]string{"Bad Header": "x"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "Bad Header") {
		t.Fatalf("space in header name should 400 naming the header, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(map[string]string{"authorization": "x"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "authorization") {
		t.Fatalf("authorization override should 400 naming the header, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(map[string]string{"X-Custom-Trace": "t1"}); rec.Code != http.StatusOK {
		t.Fatalf("valid header should be accepted, got %d %s", rec.Code, rec.Body.String())
	}
	if v := getProxyConfig().Headers["X-Custom-Trace"]; v != "t1" {
		t.Fatalf("valid header should be stored, got %q", v)
	}
}

func TestClineHeadersSkipsReserved(t *testing.T) {
	oldCfg := getProxyConfig()
	t.Cleanup(func() {
		proxyConfigMu.Lock()
		proxyConfig = oldCfg
		proxyConfigMu.Unlock()
	})
	proxyConfigMu.Lock()
	proxyConfig = &proxyConfigData{Strategy: "round_robin", Headers: map[string]string{
		"Authorization": "Bearer evil",
		"Content-Type":  "text/plain",
		"X-Custom":      "ok",
	}}
	proxyConfigMu.Unlock()

	h := clineHeaders("real-token", "sess")
	if got := h.Get("Authorization"); got != "Bearer real-token" {
		t.Fatalf("Authorization override must be ignored, got %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type override must be ignored, got %q", got)
	}
	if got := h.Get("X-Custom"); got != "ok" {
		t.Fatalf("custom header should pass through, got %q", got)
	}
}

// ---- P2-8 请求体限额 ----

func TestReadAdminBodyTooLarge(t *testing.T) {
	oldMax := maxAdminBodyBytes
	t.Cleanup(func() { maxAdminBodyBytes = oldMax })
	maxAdminBodyBytes = 16

	r := httptest.NewRequest("POST", "/admin/api/login", strings.NewReader(`{"password":"12345678901234567890"}`))
	rec := httptest.NewRecorder()

	body, ok := readAdminBody(rec, r)
	if ok {
		t.Fatal("oversized body should be rejected")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if body != nil {
		t.Fatalf("body should be nil on rejection")
	}
}

func TestReadAdminBodyOK(t *testing.T) {
	oldMax := maxAdminBodyBytes
	t.Cleanup(func() { maxAdminBodyBytes = oldMax })
	maxAdminBodyBytes = 1 << 20

	r := httptest.NewRequest("POST", "/admin/api/login", strings.NewReader(`{"password":"pw"}`))
	rec := httptest.NewRecorder()

	body, ok := readAdminBody(rec, r)
	if !ok || string(body) != `{"password":"pw"}` {
		t.Fatalf("normal body should pass, got %q ok=%v", body, ok)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("no response should be written on success, got %d", rec.Code)
	}
}

// 端到端：聊天端点超限请求体 → 413（需先绕过空池 401 分支）。
func TestChatBodyTooLarge(t *testing.T) {
	oldMax, oldPool := maxChatBodyBytes, pool
	t.Cleanup(func() { maxChatBodyBytes, pool = oldMax, oldPool })
	maxChatBodyBytes = 64
	// ExpiresAt 必须指向未来，否则 startProxy 预热会发起真实 token 刷新网络调用（致启动超时）
	pool = &AccountPool{Accounts: []*Account{&Account{
		AccountID: "a1", Email: "a@example.com", AccessToken: "tok",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}}, Keys: []string{}, Models: []Model{}}

	baseURL := protocolTestServer(t)
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 200)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413 (body=%s)", resp.StatusCode, body)
	}
}

// ---- P2-15 数据目录探测回退 ----

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !dirWritable(dir) {
		t.Fatal("temp dir should be writable")
	}
	if dirWritable(filepath.Join(dir, "missing-sub")) {
		t.Fatal("nonexistent dir should not be reported writable")
	}
}

func TestProbeDataDirCreatesWithPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "newdir")
	got, ok := probeDataDir(dir)
	if !ok || got != dir {
		t.Fatalf("probeDataDir = %q ok=%v, want created and selected", got, ok)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0700 {
			t.Fatalf("dir mode = %v, want 0700", fi.Mode().Perm())
		}
	}
}

func TestResolveDataDirStable(t *testing.T) {
	a := resolveDataDir()
	b := resolveDataDir()
	if a == "" || a != b {
		t.Fatalf("resolveDataDir should be stable, got %q then %q", a, b)
	}
}

// ---- P2-17 端口占用拒绝启动 ----

func TestEnsurePortFreeRefusesOccupied(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	defer ln.Close()

	err = ensurePortFree("127.0.0.1", port)
	if err == nil {
		t.Fatal("occupied port should be refused")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Fatalf("error should mention the port, got %q", err)
	}
	if !strings.Contains(err.Error(), "-port") {
		t.Fatalf("error should hint at -port flag, got %q", err)
	}

	// 空闲端口应放行
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port2 := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()
	if err := ensurePortFree("127.0.0.1", port2); err != nil {
		t.Fatalf("free port should pass, got %v", err)
	}
}
