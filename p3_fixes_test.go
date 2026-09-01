package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// ---- P3-9：随机源失败 fail-closed 与退化熵 ----

func stubBrokenCryptoRand(t *testing.T) {
	t.Helper()
	oldRead := cryptoRandRead
	cryptoRandRead = func(b []byte) (int, error) { return 0, errors.New("stub: crypto/rand unavailable") }
	t.Cleanup(func() { cryptoRandRead = oldRead })
}

func TestRandomHexFailClosed(t *testing.T) {
	stubBrokenCryptoRand(t)

	// 登录密码校验通过但 token 生成失败 → 500，且无新会话
	oldPool := pool
	t.Cleanup(func() {
		pool = oldPool
		resetLoginAttempts()
	})
	pool = &AccountPool{
		Accounts: []*Account{}, Keys: []string{}, Models: []Model{},
		AdminPasswordSalt: "s", AdminPasswordHash: hashAdminPassword("s", "pw"),
	}
	resetLoginAttempts()

	adminSessionsMu.Lock()
	sessionsBefore := len(adminSessions)
	adminSessionsMu.Unlock()
	if rec := postLogin("pw"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("login with broken rand = %d, want 500", rec.Code)
	}
	adminSessionsMu.Lock()
	sessionsAfter := len(adminSessions)
	adminSessionsMu.Unlock()
	if sessionsAfter != sessionsBefore {
		t.Fatalf("failed login must not create a session (%d -> %d)", sessionsBefore, sessionsAfter)
	}

	// key 生成失败 → 500，且 Keys 不变
	oldKeys := append([]string(nil), pool.Keys...)
	r := httptest.NewRequest("POST", "http://localhost:3457/admin/api/keys/generate", nil)
	rec := httptest.NewRecorder()
	handleAdminGenerateKey(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("key gen with broken rand = %d, want 500", rec.Code)
	}
	if got := loadPool().Keys; len(got) != len(oldKeys) {
		t.Fatalf("failed key gen must not modify Keys (%d -> %d)", len(oldKeys), len(got))
	}

	// randomHex 本身返回错误而非空串
	if s, err := randomHex(8); err == nil || s != "" {
		t.Fatalf("randomHex with broken rand = (%q, %v), want error and empty string", s, err)
	}
}

func TestZenRandHexFallbackVaries(t *testing.T) {
	stubBrokenCryptoRand(t)

	a := randHex(16)
	b := randHex(16)
	if a == b {
		t.Fatalf("degraded entropy must vary between calls: %q == %q", a, b)
	}
	if a == strings.Repeat("0", 32) {
		t.Fatalf("degraded entropy must not be all zeros: %q", a)
	}
	// randIntn 退化下仍在范围内
	for i := 0; i < 100; i++ {
		if v := randIntn(7); v < 0 || v >= 7 {
			t.Fatalf("randIntn degraded = %d, out of range [0,7)", v)
		}
	}
}

// ---- P3-10：/v1 错误回显最小化 ----

func TestUpstreamClientMessage(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&zenAPIError{statusCode: 503, message: "internal detail SECRET"}, "opencode upstream returned HTTP 503"},
		{&clineAPIError{statusCode: 403, message: "SECRET body"}, "upstream returned HTTP 403"},
		{&freeModelUnavailableError{message: "no eligible accounts available for free models"},
			"no eligible accounts available for free models"},
		{&clineAccountUnavailableError{err: fmt.Errorf("account a@b.c token failed: boom")},
			"no account available for this request"},
		{&clineAccountUnavailableError{err: fmt.Errorf("upstream request canceled: %w", context.Canceled)},
			"request canceled"},
		{fmt.Errorf("SECRET plain error"), "upstream request failed"},
	}
	for i, c := range cases {
		if got := upstreamClientMessage(c.err); got != c.want {
			t.Errorf("case %d: upstreamClientMessage = %q, want %q", i, got, c.want)
		}
	}
}

func TestWriteUpstreamErrorHidesUpstreamBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, &clineAPIError{statusCode: 403, message: "SECRET upstream body for HTTP 403"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "SECRET") {
		t.Fatalf("client body must not contain upstream text: %s", body)
	}
	if !strings.Contains(body, "upstream returned HTTP 403") {
		t.Fatalf("client body should carry generic status message: %s", body)
	}

	// Retry-After 仍从错误原文解析冷却文案（P1-8 语义保持），message 仍通用
	rec = httptest.NewRecorder()
	writeUpstreamError(rec, &clineAPIError{statusCode: 429, message: "Try again in 45m SECRET"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 with cooldown text must still set Retry-After")
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("429 body must not leak upstream text: %s", rec.Body.String())
	}
}

func TestSSEUpstreamErrorSanitized(t *testing.T) {
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"message\":\"SECRET account a@b.com quota exceeded\"}}\n\n")),
		Header: make(http.Header),
	}
	rec := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "m"}
	acc := &Account{AccountID: "a1", Email: "a@example.com", Status: "active"}

	handleAnthropicStream(rec, upstream, acc, reqLog)

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"upstream_error"`) {
		t.Fatalf("SSE error payload should use fixed upstream_error type, got %s", out)
	}
	if !strings.Contains(out, "upstream returned an error during streaming") {
		t.Fatalf("SSE error payload should carry fixed message, got %s", out)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "a@b.com") {
		t.Fatalf("SSE error payload leaked upstream body: %s", out)
	}
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("should still emit an error event, got %s", out)
	}
}

// ---- P3-12：日志注入净化与 email 脱敏 ----

func TestSanitizeLog(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"normal-model", 128, "normal-model"},
		{"line1\nline2", 128, "line1\\nline2"},
		{"a\rb", 128, "a\\rb"},
		{"x\x01y\x7fz", 128, "x\\x01y\\x7fz"},
		{"tab\there", 128, "tab\\there"},
		{"模型\n名", 128, "模型\\n名"},
		{"", 128, ""},
		{"abcdefgh", 4, "abcd..."},
	}
	for i, c := range cases {
		if got := sanitizeLog(c.in, c.max); got != c.want {
			t.Errorf("case %d: sanitizeLog(%q, %d) = %q, want %q", i, c.in, c.max, got, c.want)
		}
	}
	// 伪造日志行攻击被单行化
	injected := "model\n2026/09/01 00:00:00 FAKE LOG LINE injected"
	out := sanitizeLog(injected, 128)
	if strings.Contains(out, "\n") {
		t.Fatalf("sanitizeLog must keep output single-line, got %q", out)
	}
	if !strings.Contains(out, "\\n") {
		t.Fatalf("newline should be visible-escaped, got %q", out)
	}
}

func TestEmailLogMaskedAndSingleLine(t *testing.T) {
	// truncateEmail 脱敏 + sanitizeLog 单行化组合
	in := "a@exa\nFAKE\rLINE@mple.com"
	out := sanitizeLog(truncateEmail(in), 64)
	if strings.ContainsAny(out, "\n\r") {
		t.Fatalf("email log field must be single-line, got %q", out)
	}
	if strings.Contains(out, "LINE") && !strings.Contains(out, "\\r") {
		t.Fatalf("control chars should be escaped, got %q", out)
	}

	// 短 email 原样（无需脱敏）但也单行化
	if out := sanitizeLog(truncateEmail("ab@x.com\ninjected"), 64); strings.Contains(out, "\n") {
		t.Fatalf("short email with newline must be sanitized, got %q", out)
	}
}

// ---- P3-13：数值解析严格化 + account-test 显式目标 ----

func TestDecodeCursorStrict(t *testing.T) {
	// 合法 cursor 往返
	cur := encodeCursor(RequestLog{StartedAt: time.Unix(0, 1234567890).UTC(), ID: "entry-1"})
	ts, id, err := decodeCursor(cur)
	if err != nil || id != "entry-1" {
		t.Fatalf("decodeCursor(valid) = %v, %q, %v", ts, id, err)
	}

	// 尾部垃圾（Sscanf 时代会被静默接受）→ 拒绝
	bad := base64.RawURLEncoding.EncodeToString([]byte("1234567890abc|entry-1"))
	if _, _, err := decodeCursor(bad); err == nil {
		t.Fatalf("cursor with garbage timestamp must be rejected")
	}

	// 缺分隔符 / 空 id → 拒绝
	if _, _, err := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte("1234567890"))); err == nil {
		t.Fatalf("cursor without separator must be rejected")
	}
	if _, _, err := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte("1234567890|"))); err == nil {
		t.Fatalf("cursor with empty id must be rejected")
	}
}

func TestAdminRequestLogsStrictLimit(t *testing.T) {
	get := func(q string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost:3457/admin/api/request-logs"+q, nil)
		rec := httptest.NewRecorder()
		handleAdminRequestLogs(rec, r)
		return rec
	}
	// 尾部垃圾 → 400（此前 Sscanf 取前缀 50 放行）
	if rec := get("?limit=50abc"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=50abc = %d, want 400", rec.Code)
	}
	if rec := get("?limit=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400", rec.Code)
	}
	if rec := get("?limit=10"); rec.Code != http.StatusOK {
		t.Fatalf("limit=10 = %d, want 200", rec.Code)
	}
}

func TestAdminAccountTestRequiresExplicitTarget(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost:3457/admin/api/accounts/test", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handleAdminAccountTest(rec, r)
		return rec
	}

	// 空 body（旧行为：全池测试）→ 400
	if rec := post(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", rec.Code)
	}
	// 非法 JSON → 400（此前被忽略）
	if rec := post(`{bad json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d, want 400", rec.Code)
	}
	// 显式 all:true → 200（空池 results 为空）
	if rec := post(`{"all":true}`); rec.Code != http.StatusOK {
		t.Fatalf("all=true = %d, want 200", rec.Code)
	}
	// 指定不存在账号 → 404
	if rec := post(`{"accountId":"nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown accountId = %d, want 404", rec.Code)
	}
}

// ---- P3-14：truncate UTF-8 边界 / override.md 缓存与上限 / oauth 清扫 / 孤儿冷却 ----

func TestTruncateKeepsUTF8Boundary(t *testing.T) {
	s := strings.Repeat("汉", 10) // 30 字节
	got := truncate(s, 16)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated string should end with ..., got %q", got)
	}
	body := strings.TrimSuffix(got, "...")
	if strings.ContainsRune(body, utf8.RuneError) {
		t.Fatalf("truncate split a multibyte char: %q", got)
	}
	if n := utf8.RuneCountInString(body); n != 5 {
		t.Fatalf("kept runes = %d, want 5 (backed off to boundary), got %q", n, got)
	}
	// ASCII 行为不变
	if got := truncate("abcdefgh", 4); got != "abcd..." {
		t.Fatalf("ascii truncate = %q, want abcd...", got)
	}
	if got := truncate("abc", 8); got != "abc" {
		t.Fatalf("short string must be unchanged, got %q", got)
	}
}

func resetOverrideCache() {
	overrideCacheMu.Lock()
	overrideCacheOK = false
	overrideCacheMod = time.Time{}
	overrideCacheSize = 0
	overrideCacheBody = ""
	overrideCacheMu.Unlock()
}

func TestOverrideCacheReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.md")
	oldPath := overrideFilePath
	overrideFilePath = path
	resetOverrideCache()
	t.Cleanup(func() {
		overrideFilePath = oldPath
		resetOverrideCache()
	})

	if err := os.WriteFile(path, []byte("first prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadOverrideContent(); got != "first prompt" {
		t.Fatalf("first load = %q, want first prompt", got)
	}
	// 变更（不同 size）→ 重新加载
	if err := os.WriteFile(path, []byte("second prompt longer"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadOverrideContent(); got != "second prompt longer" {
		t.Fatalf("reload after change = %q, want second prompt longer", got)
	}
	// 未变 → 命中缓存（仍返回同值）
	if got := loadOverrideContent(); got != "second prompt longer" {
		t.Fatalf("cached load = %q", got)
	}
	// 文件删除 → 空串
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := loadOverrideContent(); got != "" {
		t.Fatalf("load after delete = %q, want empty", got)
	}
}

func TestOverrideSizeCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.md")
	oldPath := overrideFilePath
	overrideFilePath = path
	resetOverrideCache()
	t.Cleanup(func() {
		overrideFilePath = oldPath
		resetOverrideCache()
	})

	big := strings.Repeat("a", 300<<10) // 300KiB
	if err := os.WriteFile(path, []byte(big), 0600); err != nil {
		t.Fatal(err)
	}
	got := loadOverrideContent()
	if len(got) != overrideMaxBytes {
		t.Fatalf("capped override length = %d, want %d", len(got), overrideMaxBytes)
	}
}

func TestOAuthStatusEvictsExpired(t *testing.T) {
	oldSessions := oauthSessions
	t.Cleanup(func() { oauthSessions = oldSessions })
	oauthSessions = map[string]*oauthSessionState{
		"expired": {DeviceCode: "d1", CreatedAt: time.Now().Add(-2 * oauthSessionTTL)},
		"valid":   {DeviceCode: "d2", CreatedAt: time.Now()},
	}

	r := httptest.NewRequest("GET", "http://localhost:3457/admin/api/oauth/status?sessionId=valid", nil)
	rec := httptest.NewRecorder()
	handleOAuthStatus(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status for valid session = %d, want 200", rec.Code)
	}

	oauthSessionsMu.Lock()
	_, expiredGone := oauthSessions["expired"]
	_, validKept := oauthSessions["valid"]
	oauthSessionsMu.Unlock()
	if expiredGone {
		t.Fatalf("expired oauth session must be evicted by status query")
	}
	if !validKept {
		t.Fatalf("valid oauth session must be kept")
	}
}

func TestPruneOrphanModelCooldownsLocked(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	future := time.Now().Add(time.Hour)
	pool = &AccountPool{
		Keys: []string{},
		Models: []Model{{ID: "m1", Custom: false}},
		Accounts: []*Account{{
			AccountID: "a",
			ModelCooldowns: map[string]time.Time{
				"m1": future, // 在册模型 → 保留
				"m2": future, // 已删除模型 → 孤儿
			},
		}},
	}

	poolMu.Lock()
	n := pruneOrphanModelCooldownsLocked()
	poolMu.Unlock()
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	cd := loadPool().Accounts[0].ModelCooldowns
	if _, ok := cd["m1"]; !ok {
		t.Fatalf("live model cooldown must be kept")
	}
	if _, ok := cd["m2"]; ok {
		t.Fatalf("orphan model cooldown must be pruned")
	}
}