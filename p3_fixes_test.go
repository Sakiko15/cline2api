package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// ---- P3-4：登录按 IP 失败计数与锁定 ----

func resetLoginAttempts() {
	loginAttemptsMu.Lock()
	loginAttempts = make(map[string]*loginAttemptState)
	loginAttemptsMu.Unlock()
}

func postLogin(password string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost:3457/admin/api/login",
		strings.NewReader(`{"password":"`+password+`"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAdminLogin(rec, r)
	return rec
}

func TestAdminLoginLockoutAfterFiveFailures(t *testing.T) {
	oldPool := pool
	oldDelay := loginFailureDelay
	oldDuration := loginLockoutDuration
	loginFailureDelay = time.Millisecond
	loginLockoutDuration = 5 * time.Minute
	t.Cleanup(func() {
		pool = oldPool
		loginFailureDelay = oldDelay
		loginLockoutDuration = oldDuration
		resetLoginAttempts()
	})
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	resetLoginAttempts()

	// 5 次错误密码 → 触发锁定
	for i := 0; i < 5; i++ {
		if rec := postLogin("wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, rec.Code)
		}
	}
	// 第 6 次：即使密码正确也 429 + Retry-After
	rec := postLogin("pw")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 response must carry Retry-After")
	}
	if strings.Contains(rec.Body.String(), "login_ok") {
		t.Fatalf("locked login must not succeed")
	}
}

func TestAdminLoginSuccessResetsCounter(t *testing.T) {
	oldPool := pool
	oldDelay := loginFailureDelay
	loginFailureDelay = time.Millisecond
	t.Cleanup(func() {
		pool = oldPool
		loginFailureDelay = oldDelay
		resetLoginAttempts()
	})
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	resetLoginAttempts()

	for i := 0; i < 3; i++ {
		if rec := postLogin("wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := postLogin("pw"); rec.Code != http.StatusOK {
		t.Fatalf("correct login after 3 failures = %d, want 200", rec.Code)
	}
	// 计数已清零：再 4 次失败不应触发锁定
	for i := 0; i < 4; i++ {
		if rec := postLogin("wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := postLogin("pw"); rec.Code != http.StatusOK {
		t.Fatalf("login after 4 fresh failures = %d, want 200 (no premature lockout)", rec.Code)
	}
}

func TestAdminLockoutExpires(t *testing.T) {
	oldPool := pool
	oldDelay := loginFailureDelay
	oldDuration := loginLockoutDuration
	loginFailureDelay = time.Millisecond
	loginLockoutDuration = 20 * time.Millisecond
	t.Cleanup(func() {
		pool = oldPool
		loginFailureDelay = oldDelay
		loginLockoutDuration = oldDuration
		resetLoginAttempts()
	})
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	resetLoginAttempts()

	for i := 0; i < 5; i++ {
		postLogin("wrong")
	}
	if rec := postLogin("pw"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login = %d, want 429", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec := postLogin("pw"); rec.Code == http.StatusOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lockout did not expire within 2s")
}

// ---- P3-5/P3-7：方法校验与管理面安全头 ----

func TestAdminSecurityHeaders(t *testing.T) {
	// 静态页
	r := httptest.NewRequest("GET", "http://localhost:3457/admin/", nil)
	rec := httptest.NewRecorder()
	adminStaticHandler(rec, r)
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Fatalf("admin page response missing %s", h)
		}
	}

	// JSON API（经 writeAPI）
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
	r = httptest.NewRequest("GET", "http://localhost:3457/admin/api/accounts", nil)
	rec = httptest.NewRecorder()
	handleAdminAccounts(rec, r)
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("admin API response missing X-Frame-Options DENY")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("admin API response missing CSP")
	}
}

func TestAdminLogoutPostOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost:3457/admin/api/logout", nil)
	rec := httptest.NewRecorder()
	handleAdminLogout(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout = %d, want 405", rec.Code)
	}
}

func TestExportAccountsPostOnly(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{
		Accounts: []*Account{{AccountID: "a", Email: "e@example.com", RefreshToken: "rt"}},
		Keys:     []string{}, Models: []Model{},
	}

	r := httptest.NewRequest("GET", "http://localhost:3457/admin/api/accounts/export", nil)
	rec := httptest.NewRecorder()
	handleExportAccounts(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET export = %d, want 405", rec.Code)
	}

	r = httptest.NewRequest("POST", "http://localhost:3457/admin/api/accounts/export", nil)
	rec = httptest.NewRecorder()
	handleExportAccounts(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST export = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rt") {
		t.Fatalf("POST export body missing tokens")
	}
}

func TestOpenExternalPostOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost:3457/admin/api/open-external?url=https://example.com", nil)
	rec := httptest.NewRecorder()
	handleOpenExternal(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET open-external = %d, want 405", rec.Code)
	}

	// POST 非法 scheme → 400（不真开浏览器）
	r = httptest.NewRequest("POST", "http://localhost:3457/admin/api/open-external",
		strings.NewReader(`{"url":"file:///etc/passwd"}`))
	r.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handleOpenExternal(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST open-external file:// = %d, want 400", rec.Code)
	}
}

// ---- P3-1（后半）：请求日志防抖落盘 ----

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

// ---- P3-6/P3-8：后台密码 PBKDF2 与改密校验 ----

func shrinkPBKDF2(t *testing.T) {
	t.Helper()
	old := adminPBKDF2Iterations
	adminPBKDF2Iterations = 1000
	t.Cleanup(func() { adminPBKDF2Iterations = old })
}

func TestAdminPasswordPBKDF2RoundTrip(t *testing.T) {
	shrinkPBKDF2(t)
	hash, salt, err := newAdminPasswordHash("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	if salt != "" {
		t.Fatalf("new-format salt must be empty, got %q", salt)
	}
	if !strings.HasPrefix(hash, adminPasswordHashPrefix) {
		t.Fatalf("hash = %q, want pbkdf2 prefix", hash)
	}
	if !verifyPBKDF2Hash(hash, "s3cret!") {
		t.Fatalf("correct password must verify")
	}
	if verifyPBKDF2Hash(hash, "wrong") {
		t.Fatalf("wrong password must not verify")
	}
}

func TestVerifyAdminPasswordLegacyFormat(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	if ok, legacy := verifyAdminPassword("pw"); !ok || !legacy {
		t.Fatalf("legacy password: ok=%v legacy=%v, want true/true", ok, legacy)
	}
	if ok, _ := verifyAdminPassword("nope"); ok {
		t.Fatalf("wrong password must not verify")
	}
}

func TestAdminLoginMigratesLegacyHash(t *testing.T) {
	shrinkPBKDF2(t)
	oldPool := pool
	oldDelay := loginFailureDelay
	loginFailureDelay = time.Millisecond
	t.Cleanup(func() {
		pool = oldPool
		loginFailureDelay = oldDelay
		resetLoginAttempts()
	})
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	resetLoginAttempts()

	if rec := postLogin("pw"); rec.Code != http.StatusOK {
		t.Fatalf("legacy login = %d, want 200", rec.Code)
	}
	p := loadPool()
	if !strings.HasPrefix(p.AdminPasswordHash, adminPasswordHashPrefix) {
		t.Fatalf("hash not migrated after login: %q", p.AdminPasswordHash)
	}
	if p.AdminPasswordSalt != "" {
		t.Fatalf("salt must be cleared after migration, got %q", p.AdminPasswordSalt)
	}
	if ok, legacy := verifyAdminPassword("pw"); !ok || legacy {
		t.Fatalf("post-migration verify: ok=%v legacy=%v, want true/false", ok, legacy)
	}
	if ok, _ := verifyAdminPassword("wrong"); ok {
		t.Fatalf("wrong password must not verify after migration")
	}
}

func passwordPost(body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost:3457/admin/api/password", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAdminPassword(rec, r)
	return rec
}

func TestAdminPasswordChangeRequiresOldPassword(t *testing.T) {
	shrinkPBKDF2(t)
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	legacyHash := pool.AdminPasswordHash

	// 缺旧密码 → 400
	if rec := passwordPost(`{"password":"newpass"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing oldPassword = %d, want 400", rec.Code)
	}
	// 旧密码错误 → 400，哈希不变
	if rec := passwordPost(`{"oldPassword":"bad","password":"newpass"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong oldPassword = %d, want 400", rec.Code)
	}
	if loadPool().AdminPasswordHash != legacyHash {
		t.Fatalf("failed change must not touch hash")
	}
	// 旧密码正确 → 200，哈希迁移为 PBKDF2，会话被清空
	adminSessionsMu.Lock()
	adminSessions["stale"] = time.Now().Add(time.Hour)
	adminSessionsMu.Unlock()
	if rec := passwordPost(`{"oldPassword":"pw","password":"newpass"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid change = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(loadPool().AdminPasswordHash, adminPasswordHashPrefix) {
		t.Fatalf("hash after change = %q, want pbkdf2", loadPool().AdminPasswordHash)
	}
	adminSessionsMu.Lock()
	sessions := len(adminSessions)
	adminSessionsMu.Unlock()
	if sessions != 0 {
		t.Fatalf("password change must clear sessions, %d left", sessions)
	}
	if ok, _ := verifyAdminPassword("newpass"); !ok {
		t.Fatalf("new password must verify")
	}
}

func TestAdminPasswordEmptyRejected(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	legacyHash := pool.AdminPasswordHash

	// 空新密码（无论是否带旧密码）→ 400，哈希不变
	if rec := passwordPost(`{"oldPassword":"pw","password":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty password with old = %d, want 400", rec.Code)
	}
	if rec := passwordPost(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", rec.Code)
	}
	if loadPool().AdminPasswordHash != legacyHash {
		t.Fatalf("rejected change must not touch hash")
	}
}

func TestSetAdminPasswordUsesPBKDF2(t *testing.T) {
	shrinkPBKDF2(t)
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}

	if err := setAdminPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	p := loadPool()
	if !strings.HasPrefix(p.AdminPasswordHash, adminPasswordHashPrefix) {
		t.Fatalf("hash = %q, want pbkdf2 prefix", p.AdminPasswordHash)
	}
	if p.AdminPasswordSalt != "" {
		t.Fatalf("salt = %q, want empty for new format", p.AdminPasswordSalt)
	}
	if ok, legacy := verifyAdminPassword("hunter2"); !ok || legacy {
		t.Fatalf("verify: ok=%v legacy=%v, want true/false", ok, legacy)
	}
	// Go 内部清除路径仍可用（HTTP 入口已移除）
	if err := setAdminPassword(""); err != nil {
		t.Fatal(err)
	}
	if loadPool().AdminPasswordHash != "" {
		t.Fatalf("clear must empty hash")
	}
}