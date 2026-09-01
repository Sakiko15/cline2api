package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- P3-1：池/请求日志脏标记+防抖落盘 ----

func poolFileHasKey(t *testing.T, key string) bool {
	t.Helper()
	data, err := os.ReadFile(poolPath)
	if err != nil {
		return false
	}
	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		return false
	}
	for _, k := range p.Keys {
		if k == key {
			return true
		}
	}
	return false
}

func addKeyAndMarkDirty(key string) {
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	markPoolDirtyLocked()
	poolMu.Unlock()
}

func TestMarkPoolDirtyDebouncedFlush(t *testing.T) {
	old := poolDebouncer.delay
	poolDebouncer.delay = 10 * time.Millisecond
	t.Cleanup(func() { poolDebouncer.delay = old })

	_ = loadPool()
	// 清掉可能遗留的脏状态，避免上一测试的 pending timer 提前写出本测试的变更
	poolMu.Lock()
	poolDirty = false
	poolMu.Unlock()

	const key = "p3-debounce-key"
	addKeyAndMarkDirty(key)
	if poolFileHasKey(t, key) {
		t.Fatalf("dirty change must not be persisted before debounce fires")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if poolFileHasKey(t, key) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("debounced flush did not persist change within 2s")
}

func TestFlushPoolDirtyPersistsImmediately(t *testing.T) {
	_ = loadPool()
	const key = "p3-flush-now-key"
	addKeyAndMarkDirty(key)
	flushPoolDirty()
	if !poolFileHasKey(t, key) {
		t.Fatalf("flushPoolDirty must persist dirty change immediately")
	}
}

func TestSyncSaveClearsDirtyFlag(t *testing.T) {
	_ = loadPool()
	addKeyAndMarkDirty("p3-sync-clears-key")
	savePool()
	poolMu.Lock()
	dirty := poolDirty
	poolMu.Unlock()
	if dirty {
		t.Fatalf("synchronous savePool must clear dirty flag")
	}
}

func TestSaveFailureKeepsDirty(t *testing.T) {
	_ = loadPool()
	// 把 poolPath 指到一个「以文件为父目录」的非法路径，使 WriteFile 失败（跨平台）
	oldPath := poolPath
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	poolPath = filepath.Join(blocker, "sub", ".cline-accounts.json")
	t.Cleanup(func() { poolPath = oldPath })

	addKeyAndMarkDirty("p3-fail-keep-dirty")
	flushPoolDirty()

	poolMu.Lock()
	dirty := poolDirty
	poolMu.Unlock()
	if !dirty {
		t.Fatalf("failed write must keep dirty flag for retry")
	}
}

// ---- P3-2：round-robin 全局计数器与过滤列表长度解耦 ----

func TestRoundRobinIndependentOfFilteredList(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
	})

	accounts := []*Account{
		{AccountID: "a1", Status: "active"},
		{AccountID: "a2", Status: "active", ModelCooldowns: map[string]time.Time{
			"cool-model": time.Now().Add(time.Hour),
		}},
		{AccountID: "a3", Status: "active"},
	}
	pool = &AccountPool{Accounts: accounts}
	cfg := defaultProxyConfig()
	cfg.Strategy = "round_robin"
	setProxyConfig(cfg)
	rrCounter.Store(0)

	// eligible 列表恒为 2 个（a2 被模型冷却剔除）；连续 pick 6 次应严格交替
	var got []string
	for i := 0; i < 6; i++ {
		acc := pickAccountForModelWithFallback("cool-model", false)
		if acc == nil {
			t.Fatalf("pick %d returned nil", i)
		}
		got = append(got, acc.AccountID)
	}
	want := []string{"a1", "a3", "a1", "a3", "a1", "a3"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("pick %d = %s, want %s (sequence %v)", i, got[i], w, got)
		}
	}
}

// ---- P3-3/P3-11：health 精简 + 空池守卫实时化 ----

func TestHealthMinimalBody(t *testing.T) {
	baseURL := protocolTestServer(t)
	for _, path := range []string{"/health", "/v1/health"} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("GET %s body unparseable: %v", path, err)
		}
		if len(m) != 1 || m["status"] != "ok" {
			t.Fatalf("GET %s body = %s, want only {\"status\":\"ok\"}", path, body)
		}
	}
}

func TestChatRejectsOnlyWhenPoolEmpty(t *testing.T) {
	baseURL := protocolTestServer(t)

	// 空池：401
	oldPool := pool
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("POST empty pool: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty pool POST = %d, want 401", resp.StatusCode)
	}

	// 仅有冷却账号的池：守卫放行（不再被启动快照误拦），由上游返回决定结果
	cooldownAcc := &Account{
		AccountID:   "cooldown-only",
		Email:       "cool@example.com",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{cooldownAcc}, Keys: []string{}, Models: []Model{}}
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		httpClient.Transport = oldTransport
	})
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	resp, err = http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"some-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST cooldown-only pool: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("cooldown-only pool POST = %d, want 200; body=%s", resp.StatusCode, body)
	}
}

func TestAppendRequestLogDeferredPersist(t *testing.T) {
	oldDelay := requestLogsDebouncer.delay
	oldLogs := requestLogs
	requestLogsDebouncer.delay = 10 * time.Millisecond
	t.Cleanup(func() {
		requestLogsDebouncer.delay = oldDelay
		requestLogs = oldLogs
	})

	entry := RequestLog{ID: "p3_defer_1", StartedAt: time.Now(), Model: "m", Protocol: "openai"}
	appendRequestLog(entry)

	data, _ := os.ReadFile(requestLogsPath)
	if data != nil && len(data) > 0 {
		var entries []RequestLog
		if json.Unmarshal(data, &entries) == nil {
			for _, e := range entries {
				if e.ID == "p3_defer_1" {
					t.Fatalf("appended entry must not be persisted before debounce fires")
				}
			}
		}
	}

	flushRequestLogsDirty()
	data, err := os.ReadFile(requestLogsPath)
	if err != nil {
		t.Fatalf("flush did not write log file: %v", err)
	}
	var entries []RequestLog
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("flush wrote unparseable log file: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.ID == "p3_defer_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("flushRequestLogsDirty must persist appended entry")
	}
}