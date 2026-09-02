package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- P4-1：usage 缓存字段解析/合并 + openAIToAnthropic usage/stop_sequence 形状 ----

// jsonNum 把数值归一化为 JSON 文本，规避 Go int(0) 与反序列化 float64(0) 的类型差异
func jsonNum(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestParseTokenUsageCacheReadCreationFields(t *testing.T) {
	// Anthropic 风格：显式 cache_read/cache_creation 键
	a := parseTokenUsage(map[string]any{
		"input_tokens":                float64(100),
		"output_tokens":               float64(20),
		"cache_read_input_tokens":     float64(70),
		"cache_creation_input_tokens": float64(10),
	})
	if !a.Valid || a.CacheRead != 70 || a.CacheCreation != 10 {
		t.Fatalf("anthropic-style cache fields: %+v", a)
	}
	if a.Cached != 80 { // 既有 Cached 链行为不变
		t.Fatalf("Cached chain broken: %+v", a)
	}
	// OpenAI 风格：nested cached_tokens → CacheRead；无 CacheCreation
	o := parseTokenUsage(map[string]any{
		"prompt_tokens":     float64(120),
		"completion_tokens": float64(45),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(90),
		},
	})
	if !o.Valid || o.CacheRead != 90 || o.CacheCreation != 0 {
		t.Fatalf("openai-style cache fields: %+v", o)
	}
	// 无缓存信息时两字段为 0
	n := parseTokenUsage(map[string]any{"prompt_tokens": float64(1), "completion_tokens": float64(1)})
	if n.CacheRead != 0 || n.CacheCreation != 0 {
		t.Fatalf("plain usage should carry zero cache fields: %+v", n)
	}
}

func TestMergeTokenUsageCacheFields(t *testing.T) {
	merged := mergeTokenUsage(
		tokenUsage{Prompt: 100, CacheRead: 70, Valid: true},
		tokenUsage{Completion: 20, CacheCreation: 10, Total: 120, Valid: true},
	)
	if merged.CacheRead != 70 || merged.CacheCreation != 10 || merged.Prompt != 100 || merged.Completion != 20 {
		t.Fatalf("unexpected merged cache fields: %+v", merged)
	}
	// 无效分片不得覆盖
	merged2 := mergeTokenUsage(
		tokenUsage{Prompt: 100, CacheRead: 70, Valid: true},
		tokenUsage{CacheRead: 999, Valid: false},
	)
	if merged2.CacheRead != 70 {
		t.Fatalf("invalid fragment leaked cache fields: %+v", merged2)
	}
}

// openAIToAnthropic 输出必须能 JSON 序列化（无 nil 混排崩溃），
// usage 恒带 4 字段，input/output 仅在上游有键时覆盖。
func TestOpenAIToAnthropicUsageCacheAndStopSequence(t *testing.T) {
	mk := func(usage map[string]any, finish string) map[string]any {
		choice := map[string]any{"finish_reason": finish, "message": map[string]any{"content": "hi"}}
		out := map[string]any{"choices": []any{choice}}
		if usage != nil {
			out["usage"] = usage // openAIToAnthropic 从响应顶层读 usage
		}
		return out
	}

	// 完整 usage：缓存字段透传 + JSON round-trip
	full := openAIToAnthropic(mk(map[string]any{
		"prompt_tokens":               float64(10),
		"completion_tokens":           float64(5),
		"cache_read_input_tokens":     float64(7),
		"cache_creation_input_tokens": float64(3),
	}, "stop"))
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("response not marshalable: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}
	u := full["usage"].(map[string]any)
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(5) ||
		u["cache_read_input_tokens"] != int64(7) || u["cache_creation_input_tokens"] != int64(3) {
		t.Fatalf("usage fields wrong: %v", u)
	}
	if full["stop_sequence"] != nil {
		t.Fatalf("stop_sequence should be nil, got %v", full["stop_sequence"])
	}
	// usage 缺 prompt_tokens：基础 0 而非 null
	partial := openAIToAnthropic(mk(map[string]any{"completion_tokens": float64(5)}, "stop"))
	pu := partial["usage"].(map[string]any)
	if jsonNum(pu["input_tokens"]) != "0" || jsonNum(pu["output_tokens"]) != "5" {
		t.Fatalf("partial usage should keep zeros for missing keys: %v", pu)
	}
	if jsonNum(pu["cache_read_input_tokens"]) != "0" || jsonNum(pu["cache_creation_input_tokens"]) != "0" {
		t.Fatalf("partial usage should carry zero cache fields: %v", pu)
	}
	// 无 usage：全 0 基础形状
	none := openAIToAnthropic(mk(nil, "stop"))
	nu := none["usage"].(map[string]any)
	if len(nu) != 4 || jsonNum(nu["input_tokens"]) != "0" || jsonNum(nu["cache_read_input_tokens"]) != "0" {
		t.Fatalf("missing usage should emit 4-field zero shape: %v", nu)
	}
	// content_filter → refusal
	cf := openAIToAnthropic(mk(nil, "content_filter"))
	if cf["stop_reason"] != "refusal" {
		t.Fatalf("finish=content_filter → %v, want refusal", cf["stop_reason"])
	}
	// 空 choices 兜底分支同样带 stop_sequence/cache 字段
	empty := openAIToAnthropic(map[string]any{"choices": []any{}})
	if empty["stop_sequence"] != nil {
		t.Fatalf("empty-choices stop_sequence should be nil, got %v", empty["stop_sequence"])
	}
	eu := empty["usage"].(map[string]any)
	if jsonNum(eu["cache_read_input_tokens"]) != "0" || jsonNum(eu["cache_creation_input_tokens"]) != "0" {
		t.Fatalf("empty-choices usage should carry zero cache fields: %v", eu)
	}
}

// 流式 message_start 携带 model 与零 usage；message_delta usage 补 input_tokens。
func TestAnthropicStreamMessageStartAndDeltaUsage(t *testing.T) {
	chunks := []string{
		sseChunk(`{"choices":[{"delta":{"content":"hel"}}],"usage":{"prompt_tokens":12,"completion_tokens":2}}`),
		sseChunk(`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7}}`),
		"data: [DONE]\n\n",
	}
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Join(chunks, ""))),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	handleAnthropicStream(rec, upstream, nil, &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "claude-x"})

	events := parseSSEEvents(rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no SSE events emitted")
	}
	// message_start
	var ms map[string]any
	if err := json.Unmarshal([]byte(events[0][1]), &ms); err != nil {
		t.Fatalf("message_start unmarshal: %v", err)
	}
	if events[0][0] != "message_start" {
		t.Fatalf("first event = %s, want message_start", events[0][0])
	}
	msg := ms["message"].(map[string]any)
	if msg["model"] != "claude-x" {
		t.Fatalf("message_start.model = %v, want claude-x", msg["model"])
	}
	msu := msg["usage"].(map[string]any)
	if jsonNum(msu["input_tokens"]) != "0" || jsonNum(msu["output_tokens"]) != "0" {
		t.Fatalf("message_start.usage should be zeroed: %v", msu)
	}
	// message_delta：input_tokens 恒带，output 取最新值
	var lastDelta map[string]any
	for _, e := range events {
		if e[0] != "message_delta" {
			continue
		}
		if err := json.Unmarshal([]byte(e[1]), &lastDelta); err != nil {
			t.Fatalf("message_delta unmarshal: %v", err)
		}
	}
	if lastDelta == nil {
		t.Fatal("no message_delta event")
	}
	u := lastDelta["usage"].(map[string]any)
	if u["input_tokens"] != float64(12) || u["output_tokens"] != float64(7) {
		t.Fatalf("message_delta.usage = %v, want input=12/output=7", u)
	}
}

// ---- P4-2：user 轮 text 与 tool_result 并存时保留文本 ----

func TestAnthropicToOpenAIUserTextWithToolResult(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "result data"},
			map[string]any{"type": "text", "text": "fix the bug based on this"},
		},
	}}
	out := anthropicToOpenAI(req)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want [tool, user(text)], got %d: %v", len(msgs), msgs)
	}
	tool, _ := msgs[0].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("first message should be tool result: %v", tool)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "fix the bug based on this" {
		t.Fatalf("second message should carry user text: %v", user)
	}
}

// 纯 tool_result 轮（无 text 块）不得追加空 user 消息（守护 p1_fixes_test:139）
func TestAnthropicToOpenAIToolResultOnlyNoTrailingUser(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "ok"},
		},
	}}
	out := anthropicToOpenAI(req)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want exactly 1 tool message, got %d: %v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "tool" {
		t.Fatalf("message should be tool role: %v", m)
	}
}

// ---- P4-3：tool_result is_error 以内容前缀传递 ----

func TestAnthropicToOpenAIToolResultErrorPrefix(t *testing.T) {
	mk := func(isErr any) anthropicReq {
		req := anthropicReq{Model: "m", MaxTokens: 100}
		block := map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "boom happened"}
		if isErr != nil {
			block["is_error"] = isErr
		}
		req.Messages = []anthropicMsg{{Role: "user", Content: []any{block}}}
		return req
	}
	// true + string → 前缀
	out := anthropicToOpenAI(mk(true))
	m := out["messages"].([]any)[0].(map[string]any)
	if m["content"] != "[tool_error] boom happened" {
		t.Fatalf("is_error string content = %v, want prefixed", m["content"])
	}
	// false → 原样
	out = anthropicToOpenAI(mk(false))
	m = out["messages"].([]any)[0].(map[string]any)
	if m["content"] != "boom happened" {
		t.Fatalf("is_error=false content = %v, want unchanged", m["content"])
	}
	// 缺省 → 原样
	out = anthropicToOpenAI(mk(nil))
	m = out["messages"].([]any)[0].(map[string]any)
	if m["content"] != "boom happened" {
		t.Fatalf("absent is_error content = %v, want unchanged", m["content"])
	}
	// true + 非 string content → 不 reshaping
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{Role: "user", Content: []any{
		map[string]any{"type": "tool_result", "tool_use_id": "c", "is_error": true,
			"content": []any{map[string]any{"type": "text", "text": "structured error"}}},
	}}}
	out = anthropicToOpenAI(req)
	m = out["messages"].([]any)[0].(map[string]any)
	if _, ok := m["content"].([]any); !ok {
		t.Fatalf("array content should stay array, got %T: %v", m["content"], m["content"])
	}
}

// ---- P4-4：image 块转 OpenAI image_url ----

func TestAnthropicToOpenAIImageBase64(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what is this?"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "aGk=",
		}},
	}}}
	out := anthropicToOpenAI(req)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want single merged user message, got %d: %v", len(msgs), msgs)
	}
	m := msgs[0].(map[string]any)
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content should be 2-part array, got %T %v", m["content"], m["content"])
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("first part should be text: %v", parts[0])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("second part type = %v, want image_url", img["type"])
	}
	url := img["image_url"].(map[string]any)["url"]
	if url != "data:image/png;base64,aGk=" {
		t.Fatalf("data URL = %v", url)
	}
}

func TestAnthropicToOpenAIImageURLSource(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{Role: "user", Content: []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "url", "url": "https://example.com/a.png",
		}},
	}}}
	out := anthropicToOpenAI(req)
	parts := out["messages"].([]any)[0].(map[string]any)["content"].([]any)
	img := parts[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("part type = %v, want image_url", img["type"])
	}
	if got := img["image_url"].(map[string]any)["url"]; got != "https://example.com/a.png" {
		t.Fatalf("url = %v", got)
	}
}

// 不支持的 source 形态（file 类型）：块跳过、纯文本轮回落字符串
func TestAnthropicToOpenAIImageUnsupportedSourceSkipped(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "describe"},
		map[string]any{"type": "image", "source": map[string]any{"type": "document", "media_type": "application/pdf"}},
	}}}
	out := anthropicToOpenAI(req)
	m := out["messages"].([]any)[0].(map[string]any)
	if m["content"] != "describe" {
		t.Fatalf("content = %v, want plain string after skip", m["content"])
	}
}

// 纯图片 user 轮：parts 数组只有 image part
func TestAnthropicToOpenAIImageOnlyUserTurn(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{Role: "user", Content: []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/jpeg", "data": "anew",
		}},
	}}}
	out := anthropicToOpenAI(req)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	parts := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("want single image part, got %v", parts)
	}
}

// assistant 轮的 image 仍被跳过（OpenAI 请求无承载位），tool_calls 不受影响
func TestAnthropicToOpenAIImageAssistantSkipped(t *testing.T) {
	req := anthropicReq{Model: "m", MaxTokens: 100}
	req.Messages = []anthropicMsg{{Role: "assistant", Content: []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "aGk=",
		}},
		map[string]any{"type": "tool_use", "id": "c1", "name": "f", "input": map[string]any{"a": 1}},
	}}}
	out := anthropicToOpenAI(req)
	m := out["messages"].([]any)[0].(map[string]any)
	if _, isStr := m["content"].(string); !isStr {
		t.Fatalf("assistant content should stay string, got %T", m["content"])
	}
	if len(m["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_calls lost: %v", m["tool_calls"])
	}
}

// 端到端：/v1/messages 带图片请求 → 上游收到 image_url part
func TestAnthropicMessagesImageForwarded(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	pool = &AccountPool{Accounts: []*Account{{
		AccountID:   "img-e2e",
		Email:       "img-e2e@example.com",
		AccessToken: "img-e2e-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}}}
	setProxyConfig(defaultProxyConfig())

	var captured map[string]any
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","model":"m","choices":[{"message":{"role":"assistant","content":"seen"},"finish_reason":"stop"}]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	baseURL := protocolTestServer(t)
	payload := `{"model":"some-pool-model","max_tokens":100,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this?"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	// 上游请求体：user 轮 content 是 parts 数组，第二段是 image_url
	msgs := captured["messages"].([]any)
	var lastUser map[string]any
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i].(map[string]any); m["role"] == "user" {
			lastUser = m
			break
		}
	}
	if lastUser == nil {
		t.Fatalf("no user message in upstream body: %v", captured)
	}
	parts, ok := lastUser["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("upstream user content should be 2-part array, got %T %v", lastUser["content"], lastUser["content"])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("upstream part type = %v", img["type"])
	}
	if got := img["image_url"].(map[string]any)["url"]; got != "data:image/png;base64,aGk=" {
		t.Fatalf("upstream data URL = %v", got)
	}
}