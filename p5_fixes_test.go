package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
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

// ============ P5-3 探活 30s 超时 ============

// hungTestAccountRoundTripper 模拟尊重取消但永不响应的挂起上游。
func hungTestAccountRoundTripper() http.RoundTripper {
	return freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
}

// TestTestAccountTimesOutOnHungUpstream：上游挂起时探活在超时上界内返回失败，
// 账号保持原状态（超时走取消路径不冷却），恢复循环不再被永久卡死。
func TestTestAccountTimesOutOnHungUpstream(t *testing.T) {
	first, _ := freeTestAccounts()
	withFreeTestEnv(t, first)
	httpClient.Transport = hungTestAccountRoundTripper()

	old := testAccountTimeout
	testAccountTimeout = 100 * time.Millisecond
	t.Cleanup(func() { testAccountTimeout = old })

	done := make(chan accountTestResult, 1)
	go func() { done <- testAccount(first) }()
	select {
	case result := <-done:
		if result.OK {
			t.Fatal("testAccount reported OK on hung upstream")
		}
		if !strings.Contains(result.Error, "canceled") {
			t.Fatalf("error = %q, want cancel/deadline wording", result.Error)
		}
		if first.Status != "active" {
			t.Fatalf("account status = %q, want unchanged active", first.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("testAccount did not return within 5s on hung upstream")
	}
}

// TestAdminAccountTestHungUpstreamReturns：管理端 account-test 在挂起上游下
// 正常拿到 200 与错误结果（旧实现会永久卡死 handler）。
func TestAdminAccountTestHungUpstreamReturns(t *testing.T) {
	first, _ := freeTestAccounts()
	withFreeTestEnv(t, first)
	httpClient.Transport = hungTestAccountRoundTripper()

	old := testAccountTimeout
	testAccountTimeout = 100 * time.Millisecond
	t.Cleanup(func() { testAccountTimeout = old })

	req := httptest.NewRequest("POST", "/admin/api/accounts/test", strings.NewReader(`{"accountId":"free-one"}`))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handleAdminAccountTest(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleAdminAccountTest did not return within 5s on hung upstream")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("body = %s, want probe failure result", rec.Body.String())
	}
}

// ============ P5-4 restartListener 同址判定与状态收锁 ============

// TestRestartListenerDifferentAddrFailureKeepsOldListener：换址 bind 失败
// （目标端口被无关进程占用）必须快速报错且不动旧监听——旧实现先停旧再重试
// 500ms，失败后旧监听已关、全代理下线（零监听窗口）。
func TestRestartListenerDifferentAddrFailureKeepsOldListener(t *testing.T) {
	oldHost, oldPort := listenHost, listenPort
	oldServer := currentServer
	t.Cleanup(func() {
		listenHost, listenPort = oldHost, oldPort
		currentServer = oldServer
	})

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	targetPort := occupied.Addr().(*net.TCPAddr).Port

	// 旧监听：真实 Server 持有另一空闲端口（Addr 非空）
	oldLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldSrv := &http.Server{Addr: oldLn.Addr().String(), Handler: http.NewServeMux()}
	go oldSrv.Serve(oldLn)
	serverMu.Lock()
	currentServer = oldSrv
	serverMu.Unlock()
	t.Cleanup(func() { oldSrv.Close() })

	start := time.Now()
	err = restartListener("127.0.0.1", targetPort)
	if err == nil {
		t.Fatal("restartListener should fail when target address is occupied by a foreign process")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("failed after %v, want immediate error without shutdown-retry window", elapsed)
	}
	conn, err := net.DialTimeout("tcp", oldLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("old listener died after failed restart on different addr: %v", err)
	}
	conn.Close()
}

// TestRestartListenerConcurrentConfigRead：admin 配置读取与重启并发，
// listenHost/listenPort 收锁后 race detector 干净（-race 定向）。
func TestRestartListenerConcurrentConfigRead(t *testing.T) {
	oldHost, oldPort := listenHost, listenPort
	oldServer := currentServer
	oldMux := serverMux
	t.Cleanup(func() {
		listenHost, listenPort = oldHost, oldPort
		serverMux = oldMux
		if cu := currentServer; cu != nil && cu != oldServer {
			cu.Close()
		}
		currentServer = oldServer
	})
	serverMux = http.NewServeMux()

	// 探测空闲端口后释放，交由 restartListener 绑定
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				rec := httptest.NewRecorder()
				handleAdminConfig(rec, httptest.NewRequest("GET", "/admin/api/config", nil))
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- restartListener("127.0.0.1", port) }()

	serving := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			serving = true
			break
		}
	}
	close(stop)
	readers.Wait()
	if !serving {
		t.Fatal("restartListener did not come up on free port")
	}
	select {
	case err := <-done:
		t.Fatalf("restartListener returned while serving: %v", err)
	default:
	}
}

// ============ P5-5 token 刷新单飞 ============

// refreshFakeTransport 返回固定刷新响应的 authClient 伪造。
func refreshFakeTransport(status int, body string, delay time.Duration, calls *int32) http.RoundTripper {
	return freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		return freeTestResponse(req, status, body), nil
	})
}

func refreshOKBody() string {
	return fmt.Sprintf(`{"data":{"accessToken":"fresh","refreshToken":"rt2","expiresAt":%d}}`,
		time.Now().Add(time.Hour).UnixMilli())
}

// TestEnsureAccountTokenSingleFlight：过期 token × 8 并发，刷新端点恰好命中
// 一次，全部拿到新 token（P5-5：修复 refresh grant 轮换下的并发双刷误杀）。
func TestEnsureAccountTokenSingleFlight(t *testing.T) {
	acc := &Account{
		AccountID:    "sf-one",
		Email:        "sf@example.com",
		RefreshToken: "rt",
		AccessToken:  "workos:stale",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		Status:       "active",
	}
	withFreeTestEnv(t, acc)

	var calls int32
	oldAuth := authClient.Transport
	t.Cleanup(func() { authClient.Transport = oldAuth })
	authClient.Transport = refreshFakeTransport(http.StatusOK, refreshOKBody(), 20*time.Millisecond, &calls)

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = ensureAccountToken(acc)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1 (single-flight)", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if results[i] != "workos:fresh" {
			t.Fatalf("caller %d token = %q, want workos:fresh", i, results[i])
		}
	}
}

// TestEnsureAccountTokenFailureSerializedRetries：刷新恒 401 时并发调用方
// 全部拿到错误、各自串行重试一次（总次数 = 调用方数，非静默去重），账号 expired。
func TestEnsureAccountTokenFailureSerializedRetries(t *testing.T) {
	acc := &Account{
		AccountID:    "sf-fail",
		Email:        "sff@example.com",
		RefreshToken: "rt",
		AccessToken:  "workos:stale",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		Status:       "active",
	}
	withFreeTestEnv(t, acc)

	var calls int32
	oldAuth := authClient.Transport
	t.Cleanup(func() { authClient.Transport = oldAuth })
	authClient.Transport = refreshFakeTransport(http.StatusUnauthorized, `{"error":"invalid_grant"}`, 0, &calls)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = ensureAccountToken(acc)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != n {
		t.Fatalf("refresh calls = %d, want %d (each waiter retries once)", got, n)
	}
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			t.Fatalf("caller %d expected error", i)
		}
	}
	if acc.Status != "expired" {
		t.Fatalf("account status = %q, want expired after 401 rejection", acc.Status)
	}
}

// TestEnsureAccountTokenSnapshotNoRace：并发写 token（模拟刷新完成）与
// ensureAccountToken 快照读，-race 干净（-race 定向）。
func TestEnsureAccountTokenSnapshotNoRace(t *testing.T) {
	acc := &Account{
		AccountID:    "sf-race",
		Email:        "sfr@example.com",
		RefreshToken: "rt",
		AccessToken:  "workos:initial",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "active",
	}
	withFreeTestEnv(t, acc)

	oldAuth := authClient.Transport
	t.Cleanup(func() { authClient.Transport = oldAuth })
	authClient.Transport = refreshFakeTransport(http.StatusOK, refreshOKBody(), 0, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				poolMu.Lock()
				acc.AccessToken = fmt.Sprintf("workos:w%d", i)
				acc.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
				poolMu.Unlock()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := ensureAccountToken(acc); err != nil {
					t.Errorf("ensureAccountToken: %v", err)
					return
				}
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}