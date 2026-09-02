package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ============ P5-1 后台协程 panic 防护 ============

func TestSafeRunRecoversPanic(t *testing.T) {
	reached := false
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped safeRun: %v", r)
		}
		if !reached {
			t.Fatal("code after panic not reached")
		}
	}()
	safeRun("test-panic", func() {
		panic("boom")
	})
	reached = true
}

func TestGuardTickSurvivesPanic(t *testing.T) {
	n := 0
	for i := 0; i < 3; i++ {
		guardTick("test-tick", func() {
			n++
			if i == 1 {
				panic("tick panic")
			}
		})
	}
	if n != 3 {
		t.Errorf("loop iterations after mid-loop panic = %d, want 3", n)
	}
}

func TestSafeGoRunsFn(t *testing.T) {
	done := make(chan struct{})
	safeGo("test-go", func() { close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("safeGo goroutine never ran")
	}
}

func TestDebouncedFlushPanicUnsticksRunning(t *testing.T) {
	d := newDebouncedFlush(10*time.Millisecond, func() {
		panic("flush boom")
	})
	d.schedule()
	drained := make(chan struct{})
	go func() {
		d.drain()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("drain blocked after panicking flush (running stuck)")
	}
	// drain 后实例永久停用；另起实例验证正常落盘路径不受影响
	var called atomic.Int32
	d2 := newDebouncedFlush(10*time.Millisecond, func() { called.Add(1) })
	d2.schedule()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && called.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if called.Load() == 0 {
		t.Error("normal flush never fired after panic-recovery instance")
	}
	d2.drain()
}

func TestRunCommandReapsChild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}
	if err := runCommand("go", "version"); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
}

// ============ P5-2 free 链 tried 集合（修复 4xx/取消死循环） ============

// freeTestAccounts 构造两个活跃账号（token-one/token-two）。
func freeTestAccounts() (*Account, *Account) {
	return &Account{
			AccountID:   "free-one",
			Email:       "one@example.com",
			AccessToken: "token-one",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
			Status:      "active",
		}, &Account{
			AccountID:   "free-two",
			Email:       "two@example.com",
			AccessToken: "token-two",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
			Status:      "active",
		}
}

// withFreeTestEnv 保存/恢复 pool、代理配置、httpClient.Transport。
func withFreeTestEnv(t *testing.T, accounts ...*Account) {
	t.Helper()
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	pool = &AccountPool{Accounts: accounts}
	setProxyConfig(defaultProxyConfig())
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})
}

// freeTestResponse 构造上游响应。
func freeTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

// TestCallClineAPIFreeStopsAfterAllAccountsReturn400：fill 策略 + 单账号恒 400。
// 旧实现在此场景原地无限重选同一账号（400 不产生任何冷却），永不返回；
// 修复后每链模型试一次、链耗尽回传最终上游错误（P5-2）。
func TestCallClineAPIFreeStopsAfterAllAccountsReturn400(t *testing.T) {
	first, _ := freeTestAccounts()
	withFreeTestEnv(t, first)
	cfg := getProxyConfig()
	cfg.Strategy = "fill"
	setProxyConfig(cfg)

	var calls int32
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return freeTestResponse(req, http.StatusBadRequest, `{"error":"invalid_request"}`), nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(context.Background(), params, false)
	if err == nil {
		t.Fatal("expected error after chain exhausted with 400s")
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || apiErr.statusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want clineAPIError 400", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(len(freeModelChain)) {
		t.Fatalf("upstream calls = %d, want %d (once per chain model)", got, len(freeModelChain))
	}
}

// TestCallClineAPIFreeTriesEachAccountOncePerModel：round_robin + 两账号恒 400，
// 每链模型内每账号至多试一次（修复后总调用数 = 2×链长，不再无限）。
func TestCallClineAPIFreeTriesEachAccountOncePerModel(t *testing.T) {
	first, second := freeTestAccounts()
	withFreeTestEnv(t, first, second)
	rrCounter.Store(0) // P3-2 全局计数器：回退挑选顺序依赖列表首元素

	var attempts []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		return freeTestResponse(req, http.StatusBadRequest, `{"error":"invalid_request"}`), nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(context.Background(), params, false)
	if err == nil {
		t.Fatal("expected error after chain exhausted")
	}
	want := 2 * len(freeModelChain)
	if len(attempts) != want {
		t.Fatalf("attempts = %d, want %d", len(attempts), want)
	}
	// 每链模型块内两账号各试一次（token 不重复）
	for i := 0; i+1 < len(attempts); i += 2 {
		if attempts[i] == attempts[i+1] {
			t.Fatalf("model block %d repeated account: %v", i/2, attempts[i:i+2])
		}
	}
}

// TestCallClineAPIFreeCtxCancelReturnsPromptly：客户端取消立即返回，
// 不做徒劳遍历（旧实现在取消后仍紧凑空转），账号状态不变、不冷却。
func TestCallClineAPIFreeCtxCancelReturnsPromptly(t *testing.T) {
	first, _ := freeTestAccounts()
	withFreeTestEnv(t, first)

	var calls int32
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return freeTestResponse(req, http.StatusOK, `{"id":"ok","choices":[]}`), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(ctx, params, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 (cancel checked before pick)", got)
	}
	if first.Status != "active" {
		t.Fatalf("account status = %q, want unchanged active", first.Status)
	}
}

// TestCallClineAPIFreeFillAdvancesAfterTransientRefreshFailure：fill 策略下
// 首账号刷新暂态失败（保持 active、无冷却）后仍能推进到下一账号；
// 旧实现 fill 恒返 eligible[0]，同样死循环。
func TestCallClineAPIFreeFillAdvancesAfterTransientRefreshFailure(t *testing.T) {
	first, second := freeTestAccounts()
	// first 无 AccessToken → ensureAccountToken 触发刷新
	first.AccessToken = ""
	first.ExpiresAt = 0
	withFreeTestEnv(t, first, second)
	cfg := getProxyConfig()
	cfg.Strategy = "fill"
	setProxyConfig(cfg)

	var refreshCalls int32
	oldAuth := authClient.Transport
	t.Cleanup(func() { authClient.Transport = oldAuth })
	authClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&refreshCalls, 1)
		return freeTestResponse(req, http.StatusInternalServerError, `{"error":"boom"}`), nil
	})

	var attempts []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		return freeTestResponse(req, http.StatusOK, `{"id":"ok","choices":[]}`), nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	resp.Body.Close()
	if acc != second {
		t.Fatalf("selected account = %v, want second (advanced past transient refresh failure)", acc)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if len(attempts) < 1 || attempts[len(attempts)-1] != "token-two" {
		t.Fatalf("final attempt = %v, want token-two", attempts)
	}
	if first.Status != "active" {
		t.Fatalf("first account status = %q, want active (transient refresh failure keeps active)", first.Status)
	}
}

// ============ P5-3 探活 30s 超时（下一提交回补） ============