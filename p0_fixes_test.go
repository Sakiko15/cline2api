package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSavePoolConcurrentUnderMutation 复现 P0-1：savePool 的 Marshal 必须与
// 持锁的映射修改互斥。旧实现（无锁 Marshal）在 -race 下必然报
// "concurrent map iteration and map write"。
func TestSavePoolConcurrentUnderMutation(t *testing.T) {
	oldPoolPath := poolPath
	poolPath = filepath.Join(t.TempDir(), "pool.json")
	t.Cleanup(func() { poolPath = oldPoolPath })

	poolMu.Lock()
	pool = nil
	poolMu.Unlock()

	p := loadPool()
	_ = p
	acc := &Account{AccountID: "race-acc", Email: "race@example.com", RefreshToken: "rt", Status: "active"}
	addAccount(acc)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			recordTokenUsage(acc, "free", tokenUsage{Valid: true, Prompt: 1, Completion: 1, Total: 2})
		}()
		go func() {
			defer wg.Done()
			setModelCooldown(acc, "m", time.Now().Add(time.Minute))
		}()
		go func() {
			defer wg.Done()
			savePool()
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("pool file missing after concurrent writes: %v", err)
	}
	var check AccountPool
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("pool file is not valid JSON after concurrent writes (torn write?): %v", err)
	}
	if len(check.Accounts) != 1 {
		t.Fatalf("expected 1 account persisted, got %d", len(check.Accounts))
	}
}

// TestLoadPoolQuarantinesCorruptFile 验证损坏的池文件被隔离而非被空池覆盖销毁（P0-1 闭环）。
func TestLoadPoolQuarantinesCorruptFile(t *testing.T) {
	oldPoolPath := poolPath
	dir := t.TempDir()
	poolPath = filepath.Join(dir, "pool.json")
	t.Cleanup(func() { poolPath = oldPoolPath })

	if err := os.WriteFile(poolPath, []byte(`{"accounts":[{"accountID":"broken"`), 0600); err != nil {
		t.Fatal(err)
	}

	poolMu.Lock()
	pool = nil
	poolMu.Unlock()
	p := loadPool()

	if len(p.Accounts) != 0 {
		t.Fatalf("expected empty pool after corrupt load, got %d accounts", len(p.Accounts))
	}
	if _, err := os.Stat(poolPath + ".bad"); err != nil {
		t.Fatalf("corrupt file was not quarantined as .bad: %v", err)
	}
}

// parseSSEEvents 把 "event: X\ndata: Y\n\n" 流拆成事件对。
func parseSSEEvents(body string) [][2]string {
	var events [][2]string
	for _, block := range strings.Split(body, "\n\n") {
		var ev, data string
		for _, ln := range strings.Split(block, "\n") {
			if strings.HasPrefix(ln, "event: ") {
				ev = strings.TrimPrefix(ln, "event: ")
			}
			if strings.HasPrefix(ln, "data: ") {
				data = strings.TrimPrefix(ln, "data: ")
			}
		}
		if ev != "" {
			events = append(events, [2]string{ev, data})
		}
	}
	return events
}

// sseChunk 构造一条 OpenAI 风格 chat SSE data 行。
func sseChunk(deltaJSON string) string {
	return "data: " + deltaJSON + "\n\n"
}

// TestAnthropicStreamToolCallDeltas 复现 P0-3：分片流式的工具调用参数必须经
// input_json_delta 完整透传，content_block_start 的 input 恒为 {}。
func TestAnthropicStreamToolCallDeltas(t *testing.T) {
	fullArgs := `{"path": "/tmp/a", "limit": 5}`
	// 参数分三片流式到达：{"pa  +  th": "/tmp/a  +  , "limit": 5}
	chunks := []string{
		sseChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`),
		sseChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\": \"/tmp/a\""}}]}}]}`),
		sseChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":", \"limit\": 5}"}}]}}]}`),
		sseChunk(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}

	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Join(chunks, ""))),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	handleAnthropicStream(rec, upstream, nil, &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "test"})

	events := parseSSEEvents(rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no SSE events emitted")
	}

	var startInput string
	var deltaJSON strings.Builder
	var stopIndexes []int
	for _, e := range events {
		var data map[string]any
		if json.Unmarshal([]byte(e[1]), &data) != nil {
			continue
		}
		switch e[0] {
		case "content_block_start":
			cb, _ := data["content_block"].(map[string]any)
			if cb == nil || cb["type"] != "tool_use" {
				continue
			}
			input, _ := json.Marshal(cb["input"])
			startInput = string(input)
		case "content_block_delta":
			d, _ := data["delta"].(map[string]any)
			if d == nil || d["type"] != "input_json_delta" {
				continue
			}
			deltaJSON.WriteString(d["partial_json"].(string))
		case "content_block_stop":
			idxF, _ := data["index"].(float64)
			stopIndexes = append(stopIndexes, int(idxF))
		}
	}

	if startInput != "{}" {
		t.Fatalf("content_block_start.input should be {}, got %s", startInput)
	}
	if deltaJSON.String() != fullArgs {
		t.Fatalf("concatenated input_json_delta = %q, want %q", deltaJSON.String(), fullArgs)
	}
	if len(stopIndexes) != 1 {
		t.Fatalf("expected exactly 1 content_block_stop for the tool block, got %v", stopIndexes)
	}
	// 最终 input 必须能解析为完整对象
	var parsed map[string]any
	if err := json.Unmarshal([]byte(deltaJSON.String()), &parsed); err != nil {
		t.Fatalf("streamed args are not valid JSON: %v", err)
	}
	if parsed["path"] != "/tmp/a" {
		t.Fatalf("parsed args mismatch: %v", parsed)
	}
}

// TestAnthropicStreamTextAndToolIndexes 验证文本块与工具块的 index 不冲突（混流场景）。
func TestAnthropicStreamTextAndToolIndexes(t *testing.T) {
	chunks := []string{
		sseChunk(`{"choices":[{"delta":{"content":"Let me check."}}]}`),
		sseChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}]}}]}`),
		sseChunk(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Join(chunks, ""))),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	handleAnthropicStream(rec, upstream, nil, &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "test"})

	seen := map[int]string{} // index -> block type
	for _, e := range parseSSEEvents(rec.Body.String()) {
		if e[0] != "content_block_start" {
			continue
		}
		var data map[string]any
		if json.Unmarshal([]byte(e[1]), &data) != nil {
			continue
		}
		idxF, _ := data["index"].(float64)
		cb, _ := data["content_block"].(map[string]any)
		typ := ""
		if cb != nil {
			typ, _ = cb["type"].(string)
		}
		if prev, dup := seen[int(idxF)]; dup {
			t.Fatalf("duplicate content_block index %d (%s vs %s)", int(idxF), prev, typ)
		}
		seen[int(idxF)] = typ
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(seen))
	}
}

// TestRequireAdminAuthFailClosed 验证 P0-4：未设密码时仅本机可访问管理 API。
func TestRequireAdminAuthFailClosed(t *testing.T) {
	resetAdmin := func() {
		setAdminPassword("")
	}
	resetAdmin()
	t.Cleanup(resetAdmin)

	called := false
	next := func(w http.ResponseWriter, r *http.Request) { called = true }
	handler := requireAdminAuth(next)

	// 非回环来源（httptest.NewRequest 默认 RemoteAddr=192.0.2.1:1234）且未设密码 → 403
	req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback without password: expected 403, got %d", rec.Code)
	}
	if called {
		t.Fatal("handler must not run for non-loopback without password")
	}

	// 回环来源 → 放行
	called = false
	req = httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("loopback without password: expected pass-through, got %d (called=%v)", rec.Code, called)
	}

	// 已设密码：非回环且无会话 → 仍为 401（原有行为不变）
	setAdminPassword("secret")
	req = httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-loopback with password and no session: expected 401, got %d", rec.Code)
	}
}