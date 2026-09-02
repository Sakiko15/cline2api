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