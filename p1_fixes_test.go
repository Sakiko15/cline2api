package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- P1-8 冷却解析 ----

func TestParseCooldownUntil(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration // 与 time.Until 结果比对，容差 2s
	}{
		{"Try again in 1h 1m", 61 * time.Minute},
		{"Try again in 45m", 45 * time.Minute},   // 旧实现误读为 45h（P1-8）
		{"Try again in 2h", 2 * time.Hour},
		{"try again in 30 m", 30 * time.Minute},
		{"Try again in 8760h", 24 * time.Hour},   // 钳制上限
		{"no cooldown text here", time.Hour},     // 解析失败回退 1h
		{"Try again in 0h 0m", time.Hour},        // 0 时长回退 1h
	}
	for _, c := range cases {
		got := time.Until(parseCooldownUntil(c.body))
		if got > c.want+2*time.Second || got < c.want-2*time.Second {
			t.Errorf("parseCooldownUntil(%q) → %v, want ≈%v", c.body, got, c.want)
		}
	}
}

// ---- P1-2 Retry-After 钳制 ----

func TestClampRetryWait(t *testing.T) {
	if d := clampRetryWait(90*time.Second, maxRetryWait); d != 30*time.Second {
		t.Errorf("clamp(90s) = %v, want 30s", d)
	}
	if d := clampRetryWait(10*time.Second, maxRetryWait); d != 10*time.Second {
		t.Errorf("clamp(10s) = %v, want 10s", d)
	}
	if d := clampRetryWait(0, maxRetryWait); d != 0 {
		t.Errorf("clamp(0) = %v, want 0", d)
	}
	if d := clampRetryWait(-5*time.Second, maxRetryWait); d != 0 {
		t.Errorf("clamp(-5s) = %v, want 0", d)
	}
}

// ---- P1-6 choices 空数组不 panic ----

func TestOpenAIToAnthropicEmptyChoices(t *testing.T) {
	for _, body := range []map[string]any{
		{"choices": []any{}},
		{"choices": []any{nil}},
		{},
	} {
		out := openAIToAnthropic(body) // 旧实现在此 panic（P1-6）
		if content, ok := out["content"].([]any); !ok || len(content) == 0 {
			t.Fatalf("empty choices should produce fallback content, got %v", out["content"])
		}
		if out["stop_reason"] != "end_turn" {
			t.Errorf("stop_reason = %v, want end_turn", out["stop_reason"])
		}
	}
}

// ---- P1-12 数值钳制 ----

func TestBuildUpstreamBodyMaxTokensClamp(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{float64(4096), 4096},
		{float64(1e19), defaultMaxTokens}, // int 溢出防护
		{float64(-5), defaultMaxTokens},
		{"4096", defaultMaxTokens}, // 非数字回落默认
	}
	for _, c := range cases {
		body := buildUpstreamBody(map[string]any{"max_tokens": c.in}, false)
		if got, _ := body["max_tokens"].(int); got != c.want {
			t.Errorf("max_tokens=%v → %d, want %d", c.in, got, c.want)
		}
	}
	if got := parseTokenUsage(map[string]any{"prompt_tokens": 1e30}).Prompt; got != 0 {
		t.Errorf("overflowing usage should be rejected, got %d", got)
	}
	if got := parseTokenUsage(map[string]any{"prompt_tokens": float64(123)}).Prompt; got != 123 {
		t.Errorf("normal usage should pass, got %d", got)
	}
}

// ---- P1-13 tool_choice / stop_sequences / 多 tool_result ----

func TestAnthropicToOpenAIToolChoiceAndStop(t *testing.T) {
	zero := 0.0
	req := anthropicReq{
		ToolChoice:  jsonRaw(`{"type":"tool","name":"fs_read"}`),
		Stop:        jsonRaw(`["END"]`),
		Temperature: &zero,
	}
	out := anthropicToOpenAI(req)

	tc, ok := out["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice should map to OpenAI function shape, got %v", out["tool_choice"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil || fn["name"] != "fs_read" {
		t.Fatalf("tool_choice.function.name = %v, want fs_read", fn)
	}
	if _, ok := out["stop"].([]string); !ok {
		t.Fatalf("stop_sequences should map to stop, got %v", out["stop"])
	}
	if v, ok := out["temperature"].(float64); !ok || v != 0 {
		t.Fatalf("explicit temperature 0 must be forwarded, got %v", out["temperature"])
	}

	if got := mapAnthropicToolChoice(jsonRaw(`{"type":"any"}`)); got != "required" {
		t.Errorf("any → %v, want required", got)
	}
	if got := mapAnthropicToolChoice(jsonRaw(`{"type":"none"}`)); got != "none" {
		t.Errorf("none → %v, want none", got)
	}
	if got := mapAnthropicToolChoice(jsonRaw(`{"type":"auto"}`)); got != "auto" {
		t.Errorf("auto → %v, want auto", got)
	}
}

func TestAnthropicToOpenAIMultipleToolResults(t *testing.T) {
	req := anthropicReq{
		Model:     "m",
		MaxTokens: 100,
		Messages: []anthropicMsg{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "result-1"},
				map[string]any{"type": "tool_result", "tool_use_id": "tu2", "content": "result-2"},
			},
		}},
	}
	out := anthropicToOpenAI(req)
	msgs, _ := out["messages"].([]any)
	toolMsgs := 0
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
			toolMsgs++
		}
	}
	if toolMsgs != 2 {
		t.Fatalf("expected 2 tool messages (parallel tool results), got %d in %v", toolMsgs, msgs)
	}
}

// jsonRaw 测试辅助：构造 json.RawMessage。
func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }

// ---- P1-10 delete-all 保留非账号字段 ----

func TestDeleteAllPreservesKeysAndModels(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })

	pool = &AccountPool{
		Accounts:     []*Account{{AccountID: "a1", Status: "active"}},
		Keys:         []string{"cline_k1"},
		Models:       []Model{{ID: "m1", Source: "remote"}},
		DefaultModel: "m1",
		CurrentIdx:   3,
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/delete-all", nil)
	rec := httptest.NewRecorder()
	handleAdminDeleteAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete-all returned %d", rec.Code)
	}
	if len(pool.Accounts) != 0 {
		t.Errorf("accounts should be empty, got %d", len(pool.Accounts))
	}
	if pool.CurrentIdx != 0 {
		t.Errorf("CurrentIdx should reset, got %d", pool.CurrentIdx)
	}
	if len(pool.Keys) != 1 || pool.Keys[0] != "cline_k1" {
		t.Errorf("API keys must survive delete-all, got %v", pool.Keys)
	}
	if len(pool.Models) != 1 || pool.DefaultModel != "m1" {
		t.Errorf("models/default model must survive delete-all, got %v / %q", pool.Models, pool.DefaultModel)
	}
}

// ---- P1-9 restartListener 绑定失败不影响旧监听 ----

func TestRestartListenerBindFailureKeepsOldListener(t *testing.T) {
	oldHost, oldPort := listenHost, listenPort
	oldServer := currentServer
	t.Cleanup(func() { listenHost, listenPort, currentServer = oldHost, oldPort, oldServer })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// 该端口已被占用 → 新地址绑定必须失败，且不得影响旧监听
	if err := restartListener("127.0.0.1", port); err == nil {
		t.Fatal("restartListener should fail when the target address is occupied")
	}

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("old listener died after failed restart: %v", err)
	}
	conn.Close()
}

// ---- P1-1 refresh-all 不再持锁跨网络且能正常刷新 ----

func TestRefreshAllUpdatesAccounts(t *testing.T) {
	oldPool := pool
	oldTransport := httpClient.Transport
	oldAuthTransport := authClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		httpClient.Transport = oldTransport
		authClient.Transport = oldAuthTransport
	})

	dir := t.TempDir()
	oldPoolPath := poolPath
	poolPath = filepath.Join(dir, "pool.json")
	t.Cleanup(func() { poolPath = oldPoolPath })

	acc := &Account{
		AccountID:    "ra1",
		Email:        "ra@example.com",
		RefreshToken: "old-rt",
		AccessToken:  "old-at",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		Status:       "active",
	}
	pool = &AccountPool{Accounts: []*Account{acc}}

	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/auth/refresh":
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresAt":4102444800000}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		default:
			return nil, os.ErrInvalid
		}
	})
	httpClient.Transport = rt
	authClient.Transport = rt

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/refresh-all", nil)
	rec := httptest.NewRecorder()
	handleAdminRefreshAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("refresh-all returned %d: %s", rec.Code, rec.Body.String())
	}
	if acc.Status != "active" || acc.AccessToken != "workos:new-at" || acc.RefreshToken != "new-rt" {
		t.Fatalf("account not refreshed: %+v", acc)
	}
	data, err := os.ReadFile(poolPath)
	if err != nil || !strings.Contains(string(data), "new-rt") {
		t.Fatalf("refreshed token not persisted (err=%v)", err)
	}
}

// json 包在多个测试中使用，集中引用避免 import 抖动。
var _ = json.Marshal