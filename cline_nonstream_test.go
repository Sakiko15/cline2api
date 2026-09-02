package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// boolPtrForTest 返回 bool 指针（配置开关用例用）。
func boolPtrForTest(v bool) *bool { return &v }

// sseBody 拼装 SSE 响应体：逐块 "data: <json>"，末尾 [DONE]。
func sseBody(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// upstreamStreamOf 解析上游请求体的 stream 字段（缺省 = false）。
func upstreamStreamOf(t *testing.T, req *http.Request) bool {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read upstream body: %v", err)
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		t.Fatalf("upstream body not JSON: %s", string(raw))
	}
	s, _ := body["stream"].(bool)
	return s
}

// 聚合断言：解析合成响应并取 choices[0].message。
func decodeAggregated(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		t.Fatalf("aggregated response not JSON: %s", string(raw))
	}
	return out
}

func firstMessage(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in response: %v", out)
	}
	ch, _ := choices[0].(map[string]any)
	if ch == nil {
		t.Fatalf("choice not object: %v", choices[0])
	}
	msg, _ := ch["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("no message in choice: %v", ch)
	}
	return msg
}

// TestNonStreamViaUpstreamStreamAggregation（核心回归）：非流式客户端请求在开关
// 默认开启时改发上游 stream:true；上游对非流式 body 恒 500、对流式回 SSE——
// 修复前该场景必失败，现在由聚合器拼回标准 chat.completion JSON。
func TestNonStreamViaUpstreamStreamAggregation(t *testing.T) {
	only := walkTestAccount("agg-only", "token-agg")
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		if upstreamStreamOf(t, req) {
			sse := sseBody(
				`{"id":"gen-1","object":"chat.completion.chunk","created":1788330329,"model":"m-variant","choices":[{"index":0,"delta":{"role":"assistant","content":"He"},"finish_reason":null}]}`,
				`{"id":"gen-1","choices":[{"index":0,"delta":{"content":"llo","reasoning":"thinking..."},"finish_reason":null}]}`,
				`{"id":"gen-1","choices":[{"index":0,"delta":{"content":"","reasoning":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":2}}}`,
			)
			return fakeClineResp(req, http.StatusOK, sse), nil
		}
		return fakeClineResp(req, http.StatusInternalServerError, `{"error":"non-stream path broken"}`), nil
	})
	setupClineWalkTest(t, rt, only)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	resp, acc, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	if acc != only {
		t.Fatalf("served by %v, want only account", acc)
	}
	out := decodeAggregated(t, resp)

	if out["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", out["object"])
	}
	if out["model"] != "m-variant" {
		t.Fatalf("model = %v, want chunk model m-variant", out["model"])
	}
	msg := firstMessage(t, out)
	if msg["content"] != "Hello" {
		t.Fatalf("content = %v, want Hello", msg["content"])
	}
	if msg["reasoning"] != "thinking..." {
		t.Fatalf("reasoning = %v, want thinking...", msg["reasoning"])
	}
	choices := out["choices"].([]any)
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", ch["finish_reason"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage == nil || usage["prompt_tokens"].(float64) != 5 || usage["completion_tokens"].(float64) != 7 {
		t.Fatalf("usage not aggregated from final chunk: %v", out["usage"])
	}
}

// TestNonStreamToolCallsAggregated：tool_call 分片按 index 合并（首片段定 id/name，
// arguments 跨块追加），产出 OpenAI 非流式 tool_calls 结构。
func TestNonStreamToolCallsAggregated(t *testing.T) {
	// arguments 字符串经 json.Marshal 嵌入，避免手写转义出非法 JSON
	jsonStr := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	arg1, arg2 := `{"pa`, `th":"a.txt}"`
	chunk1 := fmt.Sprintf(`{"id":"gen-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":%s}}]}}]}`, jsonStr(arg1))
	chunk2 := fmt.Sprintf(`{"id":"gen-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`, jsonStr(arg2))

	only := walkTestAccount("agg-tools", "token-agg-tools")
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !upstreamStreamOf(t, req) {
			t.Error("upstream must receive stream:true for non-stream client request")
		}
		sse := sseBody(chunk1, chunk2,
			`{"id":"gen-3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		return fakeClineResp(req, http.StatusOK, sse), nil
	})
	setupClineWalkTest(t, rt, only)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	out := decodeAggregated(t, resp)
	msg := firstMessage(t, out)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %v, want 1 merged call", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Fatalf("tool_call id/type = %v/%v, want call_1/function", tc["id"], tc["type"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "read_file" || fn["arguments"] != arg1+arg2 {
		t.Fatalf("function = %v, want merged name+arguments %q", fn, arg1+arg2)
	}
	choices := out["choices"].([]any)
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choices[0].(map[string]any)["finish_reason"])
	}
}

// TestNonStreamPassthroughWhenUpstreamIgnoresStream：上游无视 stream:true 仍回
// 非流式 JSON → 原样透传不聚合（对上游行为鲁棒；同时保证旧 fake-JSON 测试兼容）。
func TestNonStreamPassthroughWhenUpstreamIgnoresStream(t *testing.T) {
	only := walkTestAccount("agg-passthrough", "token-passthrough")
	plain := `{"id":"gen-x","object":"chat.completion","model":"m-plain","choices":[{"index":0,"message":{"role":"assistant","content":"direct"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return fakeClineResp(req, http.StatusOK, plain), nil
	})
	setupClineWalkTest(t, rt, only)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	out := decodeAggregated(t, resp)
	if out["model"] != "m-plain" {
		t.Fatalf("model = %v, want passthrough m-plain", out["model"])
	}
	if msg := firstMessage(t, out); msg["content"] != "direct" {
		t.Fatalf("content = %v, want passthrough direct", msg["content"])
	}
}

// TestNonStreamUpstreamStreamDisabledByConfig：显式关闭 forceUpstreamStream 后
// 回退旧行为——上游收到非流式 body（无 stream 键）。
func TestNonStreamUpstreamStreamDisabledByConfig(t *testing.T) {
	only := walkTestAccount("agg-off", "token-agg-off")
	var sawStream bool
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		sawStream = upstreamStreamOf(t, req)
		return fakeClineResp(req, http.StatusOK, `{"id":"x","choices":[{"message":{"role":"assistant","content":"direct"},"finish_reason":"stop"}]}`), nil
	})
	setupClineWalkTest(t, rt, only)
	cfg := defaultProxyConfig()
	cfg.Strategy = "fill"
	cfg.ForceUpstreamStream = boolPtrForTest(false)
	setProxyConfig(cfg)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	resp.Body.Close()
	if sawStream {
		t.Fatal("upstream must receive non-stream body when forceUpstreamStream=false")
	}
}

// TestStreamErrorChunkWalksToNextAccount：聚合时流中出现 error 块 →
// clineAPIError{502}（≥500）→ callClineAPI 的 walk 换下一账号接力成功。
func TestStreamErrorChunkWalksToNextAccount(t *testing.T) {
	first := walkTestAccount("agg-a", "token-agg-a")
	second := walkTestAccount("agg-b", "token-agg-b")
	counts := map[string]int{}
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		auth := req.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "token-agg-a"):
			counts["a"]++
			return fakeClineResp(req, http.StatusOK, sseBody(`{"error":{"message":"mid-stream boom"}}`)), nil
		case strings.Contains(auth, "token-agg-b"):
			counts["b"]++
			return fakeClineResp(req, http.StatusOK, sseBody(
				`{"id":"gen-b","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			)), nil
		}
		return nil, fmt.Errorf("unexpected authorization %q", auth)
	})
	setupClineWalkTest(t, rt, first, second)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	defer resp.Body.Close()
	if acc != second {
		t.Fatalf("served by %v, want second account after mid-stream error walk", acc)
	}
	if counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("upstream hits = %v, want a:1 b:1 (agg error walks without same-account retry)", counts)
	}
	out := decodeAggregated(t, resp)
	if msg := firstMessage(t, out); msg["content"] != "ok" {
		t.Fatalf("content = %v, want ok", msg["content"])
	}
}
