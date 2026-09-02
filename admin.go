package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// In-memory OAuth login state for async browser login
var (
	oauthSessions   = make(map[string]*oauthSessionState)
	oauthSessionsMu sync.Mutex
)

type oauthSessionState struct {
	DeviceCode string
	UserCode   string
	AuthURL    string
	CreatedAt  time.Time
	Done       bool
	Success    bool
	Email      string
	Error      string
}

type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeAPI(w http.ResponseWriter, status int, resp apiResponse) {
	setAdminSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// setAdminSecurityHeaders 为管理面响应加安全头（P3-7）：管理页是内联
// script/style 的单文件（无外部资源），CSP 取 default-src 'none' + 内联放行。
func setAdminSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

// readAdminBody 限额读取管理面请求体（默认 1MB）；读取失败或超限时已写好响应，
// 调用方直接 return（P2-8）。
func readAdminBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := readBodyLimited(w, r, maxAdminBodyBytes)
	if err != nil {
		if isBodyTooLarge(err) {
			writeAPI(w, http.StatusRequestEntityTooLarge, apiResponse{Error: tAPI(r, "invalid_request_body")})
		} else {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		}
		return nil, false
	}
	return body, true
}

// 管理后台登录会话（内存态，程序重启后需重新登录）。
var (
	adminSessions   = make(map[string]time.Time)
	adminSessionsMu sync.Mutex
)

const (
	adminSessionCookie = "cline_admin_session"
	adminSessionTTL    = 24 * time.Hour
	// oauthSessionTTL OAuth 设备登录会话的最长存活时间；handleOAuthStart 懒清扫过期项
	oauthSessionTTL = 30 * time.Minute
)

// evictExpiredOAuthSessions 清理超过 TTL 的 OAuth 会话（调用方需持锁）。
func evictExpiredOAuthSessionsLocked() {
	now := time.Now()
	for id, s := range oauthSessions {
		if now.Sub(s.CreatedAt) > oauthSessionTTL {
			delete(oauthSessions, id)
		}
	}
}

// 登录防爆破（P3-4）：按源 IP 记连续失败次数，达到阈值锁定一段时间。
// 锁定期内不执行密码校验（PBKDF2 后单次校验 ~100ms+，防 CPU 耗尽）。
type loginAttemptState struct {
	fails       int
	lockedUntil time.Time
}

var (
	loginAttempts   = make(map[string]*loginAttemptState)
	loginAttemptsMu sync.Mutex
)

const (
	loginMaxConsecutiveFails = 5
	loginLockoutSweepMax     = 512 // map 惰性清理的单次上限，防异常增长
)

var (
	loginLockoutDuration = 5 * time.Minute     // var 便于测试收缩
	loginFailureDelay    = 500 * time.Millisecond
)

// clientIP 提取请求源 IP（不用可伪造的 X-Forwarded-For）。
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// sweepLoginAttemptsLocked 全量清扫空置/过期项，并把 map 压回硬上限
// 2×loginLockoutSweepMax（P5-9：旧实现 len<=512 门限在超限后永不清扫，
// 恶意流量可无界增长）。超限时按 lockedUntil 最早逐出——零值（未锁定的
// 连败计数）排最前，先于任何活跃锁被逐出。
func sweepLoginAttemptsLocked(now time.Time) {
	for k, st := range loginAttempts {
		if st.lockedUntil.IsZero() && st.fails == 0 {
			delete(loginAttempts, k)
		} else if !st.lockedUntil.IsZero() && now.After(st.lockedUntil) {
			delete(loginAttempts, k)
		}
	}
	excess := len(loginAttempts) - 2*loginLockoutSweepMax
	if excess <= 0 {
		return
	}
	type ipLock struct {
		ip    string
		until time.Time
	}
	all := make([]ipLock, 0, len(loginAttempts))
	for k, st := range loginAttempts {
		all = append(all, ipLock{k, st.lockedUntil})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].until.Before(all[j].until) })
	for i := 0; i < excess; i++ {
		delete(loginAttempts, all[i].ip)
	}
}

// loginIsLocked 报告该 IP 是否处于锁定期；到期顺手清零并全量清扫过期项。
func loginIsLocked(ip string) bool {
	now := time.Now()
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	sweepLoginAttemptsLocked(now)
	st, ok := loginAttempts[ip]
	if !ok {
		return false
	}
	if !st.lockedUntil.IsZero() {
		if now.Before(st.lockedUntil) {
			return true
		}
		st.lockedUntil = time.Time{}
		st.fails = 0
	}
	return false
}

// loginLockRemainingSeconds 返回该 IP 剩余锁定秒数（未锁定为 0）。
func loginLockRemainingSeconds(ip string) int {
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	st, ok := loginAttempts[ip]
	if !ok || st.lockedUntil.IsZero() {
		return 0
	}
	remain := time.Until(st.lockedUntil)
	if remain <= 0 {
		return 0
	}
	return int(remain.Seconds()) + 1
}

func recordLoginFailure(ip string) {
	now := time.Now()
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	st, ok := loginAttempts[ip]
	if !ok {
		st = &loginAttemptState{}
		loginAttempts[ip] = st
	}
	if !st.lockedUntil.IsZero() && now.After(st.lockedUntil) {
		st.fails = 0
		st.lockedUntil = time.Time{}
	}
	st.fails++
	if st.fails >= loginMaxConsecutiveFails {
		st.lockedUntil = now.Add(loginLockoutDuration)
	}
}

func clearLoginFailures(ip string) {
	loginAttemptsMu.Lock()
	delete(loginAttempts, ip)
	loginAttemptsMu.Unlock()
}

func registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/", adminStaticHandler)
	// 无需登录的接口
	mux.HandleFunc("/admin/api/login", corsHandler(handleAdminLogin))
	mux.HandleFunc("/admin/api/logout", corsHandler(handleAdminLogout))
	// 其余 API 全部需要后台鉴权（设置了密码后）
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return requireAdminAuth(corsHandler(h))
	}
	mux.HandleFunc("/admin/api/accounts", auth(handleAdminAccounts))
	mux.HandleFunc("/admin/api/accounts/add", auth(handleAdminAccountAdd))
	mux.HandleFunc("/admin/api/accounts/delete", auth(handleAdminAccountDelete))
	mux.HandleFunc("/admin/api/accounts/export", auth(handleExportAccounts))
	mux.HandleFunc("/admin/api/oauth/start", auth(handleOAuthStart))
	mux.HandleFunc("/admin/api/oauth/status", auth(handleOAuthStatus))
	mux.HandleFunc("/admin/api/sso/import", auth(handleSSOImport))
	mux.HandleFunc("/admin/api/stats", auth(handleAdminStats))
	mux.HandleFunc("/admin/api/batch-import", auth(handleBatchImport))
	mux.HandleFunc("/admin/api/accounts/refresh-all", auth(handleAdminRefreshAll))
	mux.HandleFunc("/admin/api/accounts/delete-all", auth(handleAdminDeleteAll))
	mux.HandleFunc("/admin/api/accounts/reset", auth(handleAdminAccountReset))
	mux.HandleFunc("/admin/api/accounts/test", auth(handleAdminAccountTest))
	mux.HandleFunc("/admin/api/keys", auth(handleAdminGetKeys))
	mux.HandleFunc("/admin/api/keys/generate", auth(handleAdminGenerateKey))
	mux.HandleFunc("/admin/api/keys/delete", auth(handleAdminDeleteKey))
	mux.HandleFunc("/admin/api/models", auth(handleAdminModels))
	mux.HandleFunc("/admin/api/models/sync", auth(handleAdminModelSync))
	mux.HandleFunc("/admin/api/opencode/config", auth(handleOpenCodeConfig))
	mux.HandleFunc("/admin/api/opencode/config/update", auth(handleOpenCodeConfigUpdate))
	mux.HandleFunc("/admin/api/opencode/models/sync", auth(handleOpenCodeModelSync))
	mux.HandleFunc("/admin/api/models/add", auth(handleAdminModelAdd))
	mux.HandleFunc("/admin/api/models/delete", auth(handleAdminModelDelete))
	mux.HandleFunc("/admin/api/config", auth(handleAdminConfig))
	mux.HandleFunc("/admin/api/config/update", auth(handleAdminUpdateConfig))
	mux.HandleFunc("/admin/api/password", auth(handleAdminPassword))
	mux.HandleFunc("/admin/api/request-logs", auth(handleAdminRequestLogs))
	mux.HandleFunc("/admin/api/open-external", auth(handleOpenExternal))
}

// isLoopbackRequest 判断请求是否来自本机（127.0.0.1 / ::1）。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminSameOrigin 校验管理面请求的同源性（CSRF 防护，P2-2）。
// 优先信任浏览器的 Sec-Fetch-Site 头；缺失时回退比较 Origin/Referer 的 host
// 与请求 Host（反代场景兼顾 X-Forwarded-Host）；两者皆无（curl 等非浏览器）放行。
func adminSameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}
	expected := []string{r.Host}
	if fh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); fh != "" {
		expected = append(expected, fh)
	}
	for _, h := range []string{"Origin", "Referer"} {
		v := strings.TrimSpace(r.Header.Get(h))
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return false
		}
		for _, e := range expected {
			if strings.EqualFold(u.Host, e) {
				return true
			}
		}
		return false
	}
	return true
}

// requireAdminAuth 后台访问鉴权中间件：未设置密码时仅允许本机访问（fail-closed，
// 防止公网部署在设置密码前暴露管理面——含明文 token 导出），否则校验会话 cookie。
func requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !adminSameOrigin(r) {
			writeAPI(w, http.StatusForbidden, apiResponse{Error: tAPI(r, "admin_csrf_blocked")})
			return
		}
		if loadPool().AdminPasswordHash == "" {
			if isLoopbackRequest(r) {
				next(w, r)
				return
			}
			writeAPI(w, http.StatusForbidden, apiResponse{Error: tAPI(r, "admin_not_bootstrapped")})
			return
		}
			c, err := r.Cookie(adminSessionCookie)
			if err != nil {
				writeAPI(w, http.StatusUnauthorized, apiResponse{Error: tAPI(r, "login_required")})
				return
			}
		adminSessionsMu.Lock()
		expiry, ok := adminSessions[c.Value]
		if ok {
			if time.Now().Before(expiry) {
				adminSessionsMu.Unlock()
				next(w, r)
				return
			}
			delete(adminSessions, c.Value)
		}
			adminSessionsMu.Unlock()
			writeAPI(w, http.StatusUnauthorized, apiResponse{Error: tAPI(r, "session_expired")})
	}
}

// hashAdminPassword 生成加盐密码哈希：hex(sha256(salt+password))。
// 仅用于存量 legacy 格式（P3-8 迁移前），新密码一律走 newAdminPasswordHash。
func hashAdminPassword(saltHex, password string) string {
	sum := sha256.Sum256([]byte(saltHex + password))
	return hex.EncodeToString(sum[:])
}

// adminPasswordHashPrefix 标记 PBKDF2 自包含哈希格式：pbkdf2-sha256$<iter>$<saltHex>$<hashHex>。
const adminPasswordHashPrefix = "pbkdf2-sha256$"

// adminPBKDF2Iterations PBKDF2 迭代次数（OWASP 2023 下限 600k，约 100-200ms/次）。
var adminPBKDF2Iterations = 600000

// newAdminPasswordHash 为密码生成 PBKDF2 哈希（自包含单字段，AdminPasswordSalt 置空）。
// 随机数或 KDF 失败返回 error（fail-closed，绝不回退到弱哈希）。
func newAdminPasswordHash(password string) (hash, salt string, err error) {
	saltBytes := make([]byte, 16)
	if _, err := cryptoRandRead(saltBytes); err != nil {
		return "", "", fmt.Errorf("generate salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, saltBytes, adminPBKDF2Iterations, sha256.Size)
	if err != nil {
		return "", "", fmt.Errorf("derive password hash: %w", err)
	}
	hash = fmt.Sprintf("%s%d$%s$%s", adminPasswordHashPrefix, adminPBKDF2Iterations,
		hex.EncodeToString(saltBytes), hex.EncodeToString(key))
	return hash, "", nil
}

// verifyPBKDF2Hash 校验 pbkdf2-sha256$<iter>$<saltHex>$<hashHex> 格式哈希（常量时间比较）。
// 迭代次数/盐/哈希任一解析失败即拒绝；迭代次数设上界防数据文件损坏导致的 KDF 拖垮登录。
func verifyPBKDF2Hash(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 10_000_000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// setAdminPassword 设置/修改/清除后台密码（空 = 清除，仅供 Go 内部调用；HTTP 入口
// 已移除清除路径），并清空所有会话强制重新登录。PBKDF2 格式（P3-8）；
// 随机数/KDF 失败返回 error 且不改动现有哈希（fail-closed）。
func setAdminPassword(password string) error {
	newHash := ""
	if password != "" {
		h, _, err := newAdminPasswordHash(password) // KDF 在锁外（100-200ms 不能持锁）
		if err != nil {
			return err
		}
		newHash = h
	}
	p := loadPool()
	poolMu.Lock()
	p.AdminPasswordHash = newHash
	p.AdminPasswordSalt = ""
	poolMu.Unlock()
	savePool()
	adminSessionsMu.Lock()
	adminSessions = make(map[string]time.Time)
	adminSessionsMu.Unlock()
	return nil
}

// adminAutoPassword 是否允许启动时自动生成随机管理密码（测试 seam：TestMain
// 关闭，避免协议测试拉起完整代理时向共享临时池文件写入哈希造成跨测试耦合）。
var adminAutoPassword = true

// bootstrapAdminPassword 启动时的一次性管理密码引导（startProxy 在注册管理
// 路由前调用）。优先级：既有哈希 > CLINE_ADMIN_PASSWORD > 随机生成。
// 随机密码按 8-8-8-8 分组打印到日志（分组串即登录密码，128 位熵），仅生成
// 时显示一次——落盘的是 PBKDF2 哈希，明文不可恢复。生成失败保持无密码态，
// requireAdminAuth 的 fail-closed 分支继续拒绝非本机访问。
func bootstrapAdminPassword() {
	if loadPool().AdminPasswordHash != "" {
		return
	}
	if envPwd := os.Getenv("CLINE_ADMIN_PASSWORD"); envPwd != "" {
		if err := setAdminPassword(envPwd); err != nil {
			log.Printf("admin password bootstrap failed: %v", err)
		} else {
			log.Printf("admin password bootstrapped from CLINE_ADMIN_PASSWORD environment variable")
		}
		return
	}
	if !adminAutoPassword {
		return
	}
	raw, err := randomHex(16)
	if err != nil {
		log.Printf("admin password generation failed: %v (non-loopback admin access stays denied)", err)
		return
	}
	grouped := strings.Join([]string{raw[0:8], raw[8:16], raw[16:24], raw[24:32]}, "-")
	if err := setAdminPassword(grouped); err != nil {
		log.Printf("admin password bootstrap failed: %v", err)
		return
	}
	log.Printf("  admin panel initial password: %s (sign in at /admin/ and change it after first login)", grouped)
}

// verifyAdminPassword 校验后台密码（未设置密码时返回 false）。
// 双格式兼容：PBKDF2 自包含格式或存量单轮 SHA-256（legacy）。
// legacy 比较改用常量时间比较（P3-6）。KDF/哈希计算在 poolMu 外。
// 返回值 legacy 表示存量哈希仍是旧格式（调用方可择机迁移）。
func verifyAdminPassword(password string) (ok bool, legacy bool) {
	p := loadPool()
	poolMu.Lock()
	storedHash, storedSalt := p.AdminPasswordHash, p.AdminPasswordSalt
	poolMu.Unlock()
	if storedHash == "" {
		return false, false
	}
	if strings.HasPrefix(storedHash, adminPasswordHashPrefix) {
		return verifyPBKDF2Hash(storedHash, password), false
	}
	got := []byte(hashAdminPassword(storedSalt, password))
	return subtle.ConstantTimeCompare(got, []byte(storedHash)) == 1, true
}

// upgradeAdminPasswordHash 将存量单轮 SHA-256 哈希透明迁移为 PBKDF2（登录成功后调用）。
// KDF 在锁外计算；入锁后复查仍为 legacy 才写回（幂等，并发登录只迁移一次）。
func upgradeAdminPasswordHash(password string) {
	hash, _, err := newAdminPasswordHash(password)
	if err != nil {
		log.Printf("admin password hash upgrade skipped: %v", err)
		return
	}
	p := loadPool()
	poolMu.Lock()
	if p.AdminPasswordHash == "" || strings.HasPrefix(p.AdminPasswordHash, adminPasswordHashPrefix) {
		poolMu.Unlock()
		return
	}
	p.AdminPasswordHash = hash
	p.AdminPasswordSalt = ""
	poolMu.Unlock()
	savePool()
	log.Printf("admin password hash upgraded to PBKDF2")
}

// cryptoRandRead 随机源 seam（P3-9）：测试可注入故障验证 fail-closed 路径。
var cryptoRandRead = rand.Read

// randomHex 生成 n 字节随机数的 hex 字符串。
// P3-9：随机源失败返回 error（调用方 fail-closed），绝不再返回空串——
// 空串会话键可被伪造、空 API key 等于无鉴权。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := cryptoRandRead(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// forbiddenUpstreamHeaders 不允许经管理面覆盖的请求头——它们由代理自身生成
// （鉴权/协议语义），覆盖会破坏对上游的请求（P2-16）。
var forbiddenUpstreamHeaders = map[string]bool{
	"Authorization":  true,
	"Content-Type":   true,
	"Host":           true,
	"Content-Length": true,
}

// validHeaderName 校验头名为合法的 RFC 7230 token（排除分隔符与非可见 ASCII）。
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= ' ' || c >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={}", rune(c)) {
			return false
		}
	}
	return true
}

// requestIsHTTPS 判断请求是否经由 HTTPS 到达（直连 TLS 或反代转发 X-Forwarded-Proto）。
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// POST /admin/api/login  body: {password}
func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
		if !adminSameOrigin(r) {
			writeAPI(w, http.StatusForbidden, apiResponse{Error: tAPI(r, "admin_csrf_blocked")})
			return
		}
		if r.Method != "POST" {
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
			return
		}
		body, ok := readAdminBody(w, r)
		if !ok {
			return
		}
		defer r.Body.Close()
		var req struct {
			Password string `json:"password"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
			return
		}
		if loadPool().AdminPasswordHash == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "password_not_enabled")})
			return
		}
		// 锁定检查先于密码校验：锁定期不跑 KDF（P3-4）
		ip := clientIP(r)
		if loginIsLocked(ip) {
			w.Header().Set("Retry-After", strconv.Itoa(loginLockRemainingSeconds(ip)))
			writeAPI(w, http.StatusTooManyRequests, apiResponse{Error: tAPI(r, "too_many_attempts")})
			return
		}
		ok, legacy := verifyAdminPassword(req.Password)
		if !ok {
			recordLoginFailure(ip)
			time.Sleep(loginFailureDelay) // 防爆破（未触发锁定时的额外摩擦）
			writeAPI(w, http.StatusUnauthorized, apiResponse{Error: tAPI(r, "wrong_password")})
			return
		}
		clearLoginFailures(ip)
		if legacy { // P3-8：存量单轮 SHA-256 哈希在首次成功登录时透明迁移
			upgradeAdminPasswordHash(req.Password)
		}
	token, err := randomHex(32)
	if err != nil { // P3-9：空串会话键可被伪造，随机源失败必须 fail-closed
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: tAPI(r, "internal_error")})
		return
	}
	adminSessionsMu.Lock()
	adminSessions[token] = time.Now().Add(adminSessionTTL)
	adminSessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(adminSessionTTL.Seconds()),
	})
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "login_ok")})
	}

	// POST /admin/api/logout
	func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
		if !adminSameOrigin(r) {
			writeAPI(w, http.StatusForbidden, apiResponse{Error: tAPI(r, "admin_csrf_blocked")})
			return
		}
		if r.Method != "POST" { // P3-5：GET 顶层导航即可清会话，必须 POST
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
			return
		}
		if c, err := r.Cookie(adminSessionCookie); err == nil {
			adminSessionsMu.Lock()
			delete(adminSessions, c.Value)
			adminSessionsMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: "", Path: "/admin", MaxAge: -1,
			HttpOnly: true, Secure: requestIsHTTPS(r)})
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "logout_ok")})
	}

// POST /admin/api/password  body: {oldPassword, password}
// 已设密码时必须携带并匹配当前密码（P3-8）；空新密码 400 拒绝（清除密码入口已移除）。
func handleAdminPassword(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
			return
		}
		body, ok := readAdminBody(w, r)
		if !ok {
			return
		}
		defer r.Body.Close()
		var req struct {
			OldPassword string `json:"oldPassword"`
			Password    string `json:"password"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
			return
		}
		if req.Password == "" { // P3-8：留空清除密码的入口移除，空新密码一律拒绝
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "password_required")})
			return
		}
		if loadPool().AdminPasswordHash != "" { // 已设密码：改密需验证当前密码
			if req.OldPassword == "" {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "old_password_required")})
				return
			}
			if ok, _ := verifyAdminPassword(req.OldPassword); !ok {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "wrong_password")})
				return
			}
		}
		if err := setAdminPassword(req.Password); err != nil {
			writeAPI(w, http.StatusInternalServerError, apiResponse{Error: tAPI(r, "internal_error")})
			return
		}
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "password_updated")})
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		setAdminSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(adminHTML))
		return
	}
	http.NotFound(w, r)
}

// GET /admin/api/accounts
func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	accounts := listAccounts()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":  accounts,
			"total":     len(accounts),
			"poolIndex": loadPool().CurrentIdx,
		},
	})
}

// POST /admin/api/accounts/add  body: { refreshToken, email }
func handleAdminAccountAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

		if req.RefreshToken == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "refresh_token_required")})
			return
		}

		// Validate by refreshing
		resp, err := refreshClineToken(req.RefreshToken)
		if err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_refresh_token", err.Error())})
			return
		}

	if req.Email == "" {
		req.Email = fmt.Sprintf("user_%d", len(loadPool().Accounts)+1)
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        req.Email,
		RefreshToken: req.RefreshToken,
		AccessToken:  "workos:" + resp.Data.AccessToken,
		ExpiresAt:    parseExpiry(resp.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}

	addAccount(acc)
	log.Printf("Account added via API: %s", sanitizeLog(truncateEmail(req.Email), 64))

		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: tAPI(r, "account_added", req.Email),
		Data: map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    acc.Status,
		},
	})
}

// POST /admin/api/accounts/delete  body: { accountId }
func handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

		if req.AccountID == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "account_id_required")})
			return
		}

		if removeAccount(req.AccountID) {
			writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "account_deleted")})
		} else {
			writeAPI(w, http.StatusNotFound, apiResponse{Error: tAPI(r, "account_not_found")})
		}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}

	device, err := workosDeviceAuth()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	sessionID := fmt.Sprintf("oauth_%d", time.Now().UnixMilli())
	state := &oauthSessionState{
		DeviceCode: device.DeviceCode,
		UserCode:   device.UserCode,
		AuthURL:    authURL,
		CreatedAt:  time.Now(),
	}

	oauthSessionsMu.Lock()
	evictExpiredOAuthSessionsLocked()
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	safeGo("oauth-poll", func() {
		interval := device.Interval
		if interval < 5 {
			interval = 5
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300
		}

		workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		email := "unknown"
		if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
			email = cline.Data.UserInfo.Email
		}

		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: cline.Data.RefreshToken,
			AccessToken:  "workos:" + cline.Data.AccessToken,
			ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", sanitizeLog(truncateEmail(email), 64))
	})

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"sessionId":       sessionID,
			"verificationUri": authURL,
			"userCode":        device.UserCode,
		},
	})
}

// GET /admin/api/oauth/status?sessionId=xxx
func handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "session_id_required")})
		return
	}

	oauthSessionsMu.Lock()
	evictExpiredOAuthSessionsLocked() // P3-14：状态查询处懒清扫，未轮询 start 的孤儿会话也能被回收
	state, ok := oauthSessions[sessionID]
	if ok {
		snap := *state // 锁内拷贝快照：后台轮询 goroutine 持锁改写字段（P2-7）
		state = &snap
	}
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: tAPI(r, "session_not_found")})
		return
	}

	resp := map[string]any{
		"done":    state.Done,
		"success": state.Success,
	}
	if state.Done {
		resp["email"] = state.Email
		if !state.Success {
			resp["error"] = state.Error
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
}

// POST /admin/api/sso/import  body: { ssoCookies: string, email?: string }
func handleSSOImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "sso_cookies_required")})
		return
	}

	// SSO cookies import - try to use WorkOS device auth (requires browser)
	// For direct SSO cookie conversion, we'd need the WorkOS session cookie
	// to exchange for tokens. This is a placeholder that accepts WorkOS session
	// cookies. In practice, users should use OAuth or direct refreshToken.
	//
	// SSO cookie format expected: workos_session=xxx or similar
	lines := strings.Split(req.SSOCookies, "\n")
	imported := 0
	errors := []string{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := refreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("token %s...: %v", truncate(token, 16), err))
				continue
			}
			email := req.Email
			if email == "" {
				email = fmt.Sprintf("sso_user_%d", time.Now().UnixMilli())
			}

			acc := &Account{
				AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
				Email:        email,
				RefreshToken: token,
				AccessToken:  "workos:" + resp.Data.AccessToken,
				ExpiresAt:    parseExpiry(resp.Data.ExpiresAt) - 60000,
				Status:       "active",
				CreatedAt:    time.Now(),
			}
			addAccount(acc)
			imported++
		}
	}

	result := map[string]any{
		"imported": imported,
		"failed":   len(errors),
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: tAPI(r, "imported_accounts", imported, len(errors)),
			Data:    result,
		})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		Tokens []struct {
			RefreshToken string `json:"refreshToken"`
			Email        string `json:"email"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

		if len(req.Tokens) == 0 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "tokens_empty")})
			return
		}

	imported := 0
	errors := []string{}

	for _, t := range req.Tokens {
		if t.RefreshToken == "" {
			continue
		}
		resp, err := refreshClineToken(t.RefreshToken)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.Email, err))
			continue
		}
		email := t.Email
		if email == "" {
			email = fmt.Sprintf("batch_%d", time.Now().UnixMilli())
		}
		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: t.RefreshToken,
			AccessToken:  "workos:" + resp.Data.AccessToken,
			ExpiresAt:    parseExpiry(resp.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)
		imported++
	}

		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: tAPI(r, "imported_accounts", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// POST /admin/api/accounts/export — 导出账号为批量导入兼容格式。
// POST-only（P3-5）：顶层导航即可触发的 GET 在 SameSite=Lax 下不受 CSRF 保护。
func handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}

	p := loadPool()
	type exportToken struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	// P5-9：RefreshToken 读收进 poolMu 快照（与 token 刷新轮换的锁内写并发）
	poolMu.Lock()
	tokens := make([]exportToken, 0, len(p.Accounts))
	for _, acc := range p.Accounts {
		if acc.RefreshToken != "" {
			tokens = append(tokens, exportToken{
				RefreshToken: acc.RefreshToken,
				Email:        acc.Email,
			})
		}
	}
	poolMu.Unlock()

	setAdminSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cline-accounts-export.json"`)
	json.NewEncoder(w).Encode(map[string]any{
		"tokens":     tokens,
		"exportedAt": time.Now().Format(time.RFC3339),
	})
}

// POST /admin/api/open-external  body: {url} — 用系统默认浏览器打开外部链接。
// POST-only（P3-5）：GET 顶层导航即可触发浏览器弹窗骚扰。
func handleOpenExternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	url := req.URL
	if url == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "url_required")})
		return
	}
	// 仅允许 http/https，防止任意命令执行
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "url_http_only")})
		return
	}
	if err := openBrowser(url); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

// POST /admin/api/accounts/refresh-all
func handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	p := loadPool()
	// 仅在锁内做快照（账号指针稳定），网络刷新在锁外进行——
	// 否则一次 auth 端点挂起会让 poolMu 永久被占，整个代理停摆（P1-1）。
	poolMu.Lock()
	accs := make([]*Account, len(p.Accounts))
	copy(accs, p.Accounts)
	poolMu.Unlock()
	for _, a := range accs {
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", sanitizeLog(truncateEmail(a.Email), 64), sanitizeLog(err.Error(), 256))
		}
	}
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "tokens_refreshed")})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	poolMu.Lock()
	// 仅清空账号与轮询游标；Keys/Models/DefaultModel/管理密码/监听地址必须保留，
	// 否则所有客户端立刻 401、模型表被清空（P1-10）
	pool.Accounts = []*Account{}
	pool.CurrentIdx = 0
	poolMu.Unlock()
	savePool()
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "accounts_deleted")})
}

// POST /admin/api/accounts/reset  body: { accountId }
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

		acc := getAccountByID(req.AccountID)
		if acc == nil {
			writeAPI(w, http.StatusNotFound, apiResponse{Error: tAPI(r, "account_not_found")})
			return
		}

		// Reset status to active and refresh token, but preserve usage/token statistics.
		markAccountActive(acc)
		if err := refreshAccountToken(acc); err != nil {
			writeAPI(w, http.StatusInternalServerError, apiResponse{Error: tAPI(r, "reset_failed", err.Error())})
			return
		}

		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "account_reset")})
}

// POST /admin/api/accounts/test  body: { accountId?: "" }
func handleAdminAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
		All       bool   `json:"all"`
	}
	if err := json.Unmarshal(body, &req); err != nil { // P3-13：解析失败不再忽略
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	if req.AccountID == "" && !req.All { // P3-13：全池探活必须显式声明，防空 body 误触发全量请求
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "test_target_required")})
		return
	}

	p := loadPool()
	var targets []*Account
	if req.AccountID != "" {
		acc := getAccountByID(req.AccountID)
			if acc == nil {
				writeAPI(w, http.StatusNotFound, apiResponse{Error: tAPI(r, "account_not_found")})
				return
			}
		targets = []*Account{acc}
	} else {
		poolMu.Lock()
		targets = make([]*Account, len(p.Accounts))
		copy(targets, p.Accounts)
		poolMu.Unlock()
	}

	results := make([]accountTestResult, 0, len(targets))
	for _, acc := range targets {
		results = append(results, testAccount(acc))
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data:    map[string]any{"results": results},
	})
}

// Global proxy config (mutable via API, persisted to .cline-config.json)
var (
	proxyConfig   = loadProxyConfigFromDisk()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
	// ForceUpstreamStream 非流式客户端请求是否改发上游流式再聚合（v1.3.6）。
	// 指针语义：nil（旧配置文件无此键）= 默认开启；显式 false 才回退旧行为。
	ForceUpstreamStream *bool `json:"forceUpstreamStream,omitempty"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/3.0.47",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "3.0.47",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "3.0.47",
			"X-CORE-VERSION":     "0.0.66",
		},
	}
}

const proxyConfigPath = ".cline-config.json"

// proxyConfigFile 启动时解析的落盘路径（resolveDataPath 为纯函数，包级初始化安全；
// 变量形式便于测试重定向到临时目录）。
var proxyConfigFile = resolveDataPath(proxyConfigPath)

// loadProxyConfigFromDisk 启动时加载持久化的代理配置（轮询策略/请求头），
// 文件不存在或损坏时回退默认值。resolveDataPath 为纯函数，包级初始化安全。
func loadProxyConfigFromDisk() *proxyConfigData {
	cfg := defaultProxyConfig()
	if data, err := os.ReadFile(proxyConfigFile); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			// 坏文件隔离：否则下次保存会用默认值覆盖销毁原始配置
			quarantineFile(proxyConfigFile, err)
		}
	}
	switch cfg.Strategy {
	case "round_robin", "fill", "random":
	default:
		cfg.Strategy = "round_robin"
	}
	return cfg
}

// saveProxyConfigLocked 落盘当前配置（调用方需持有 proxyConfigMu）。
func saveProxyConfigLocked() {
	data, err := json.MarshalIndent(proxyConfig, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(proxyConfigFile, data, 0600); err != nil {
		log.Printf("proxy config save failed: %v", err)
	}
}

func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	proxyConfig = c
	saveProxyConfigLocked()
}

// cloneProxyConfig 返回当前代理配置的深拷贝（Headers 独立），供写时复制修改。
// getProxyConfig 返回的是共享指针，禁止就地修改——写方必须 clone 后经 setProxyConfig 原子替换，
// 否则与每请求 clineHeaders 的 map 遍历并发会触发致命的 "concurrent map writes"。
func cloneProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	c := *proxyConfig
	headers := make(map[string]string, len(proxyConfig.Headers))
	for k, v := range proxyConfig.Headers {
		headers[k] = v
	}
	c.Headers = headers
	return &c
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	p := loadPool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": p.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	keyHex, err := randomHex(32) // 256-bit 随机，不含时间戳可预测成分（P2-3）
	if err != nil { // P3-9："cline_" 空尾 key 等于无鉴权，随机源失败必须 fail-closed
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: tAPI(r, "internal_error")})
		return
	}
	key := "cline_" + keyHex
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "key_deleted")})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	serverMu.Lock() // listenHost/listenPort 与 restartListener 的写入互斥（P5-4）
	adminHost, adminPort := listenHost, listenPort
	serverMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":      fmt.Sprintf("%s:%d", effectiveAdminHost(adminHost), adminPort),
		"host":         adminHost,
		"strategy":     cfg.Strategy,
		"forceUpstreamStream": upstreamStreamForNonStream(cfg),
		"version":      appVersion,
		"poolPath":     poolPath,
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
		"localIPs":     detectLocalIPs(),
		"hasPassword":  loadPool().AdminPasswordHash != "",
		"dataDir":         resolveDataDir(),
		"dataDirWritable": dirWritable(resolveDataDir()),
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel?, host? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy            string            `json:"strategy"`
		Headers             map[string]string `json:"headers"`
		DefaultModel        string            `json:"defaultModel"`
		Host                string            `json:"host"`
		ForceUpstreamStream *bool             `json:"forceUpstreamStream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

	cfg := cloneProxyConfig()
	changed := false
	restarting := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			cfg.Strategy = req.Strategy
			changed = true
		default:
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_strategy")})
			return
		}
	}

	if req.Headers != nil {
		for k := range req.Headers {
			if !validHeaderName(k) {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_header_name", k)})
				return
			}
			if forbiddenUpstreamHeaders[http.CanonicalHeaderKey(k)] {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "forbidden_header", k)})
				return
			}
		}
		for k, v := range req.Headers {
			cfg.Headers[k] = v
		}
		changed = true
	}

	if req.ForceUpstreamStream != nil {
		cfg.ForceUpstreamStream = req.ForceUpstreamStream
		changed = true
	}

	if req.DefaultModel != "" {
		// 校验默认模型存在于可用模型列表中
		found := false
		for _, m := range getAllModels() {
			if m.ID == req.DefaultModel {
				found = true
				break
			}
		}
		if !found {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_default_model")})
			return
		}
		p := loadPool()
		poolMu.Lock()
		p.DefaultModel = req.DefaultModel
		poolMu.Unlock()
		savePool()
	}

	if req.Host != "" {
		// 校验监听地址：回环 / 0.0.0.0 / 本机检测到的 IP
		valid := req.Host == "127.0.0.1" || req.Host == "0.0.0.0" || req.Host == "localhost" || req.Host == "::1"
		if !valid {
			for _, ip := range detectLocalIPs() {
				if ip == req.Host {
					valid = true
					break
				}
			}
		}
		if !valid {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_host")})
			return
		}
		p := loadPool()
		poolMu.Lock()
		p.ListenHost = req.Host
		poolMu.Unlock()
		savePool()
		restarting = true
	}

	if changed {
		setProxyConfig(cfg)
	}

	if restarting {
		serverMu.Lock() // 快照当前端口，与 restartListener 的写入互斥（P5-4）
		curPort := listenPort
		serverMu.Unlock()
		// 异步重启监听（Shutdown 会等待当前请求完成，不能在 handler 内同步调用）
		safeGo("listener-restart", func() {
			if err := restartListener(req.Host, curPort); err != nil && err != http.ErrServerClosed {
				log.Printf("Listener restart failed: %v", err)
			}
		})
	}

	serverMu.Lock()
	respHost, respPort := listenHost, listenPort
	serverMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":      cfg.Strategy,
		"headers":       cfg.Headers,
		"defaultModel":  getDefaultModel(),
		"host":          respHost,
		"address":       fmt.Sprintf("%s:%d", effectiveAdminHost(respHost), respPort),
		"restarting":    restarting,
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	models := getAllModels()
	// zen 模型计费归一化：与路由判定保持一致（种子白名单兜底），避免 UI 分组与分流不一致
	for i := range models {
		if isZenSource(models[i]) && isZenFreeModel(models[i]) && models[i].Cost != "free" {
			models[i].Cost = "free"
		}
	}
	sync := getModelSyncResult()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models":   models,
		"lastSync": sync,
	}})
}

// POST /admin/api/models/add  body: { id, provider?, cost? }
func handleAdminModelAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Cost     string `json:"cost"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

		if req.ID == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "model_id_required")})
			return
		}

		// 校验不与已有模型重复
		for _, m := range getAllModels() {
			if m.ID == req.ID {
				writeAPI(w, http.StatusConflict, apiResponse{Error: tAPI(r, "model_exists")})
				return
			}
		}

	// cost 默认为 pass
	cost := req.Cost
	if cost == "" {
		cost = "pass"
	}
	// provider 可选，留空则从 ID 前缀推断
	provider := req.Provider
	if provider == "" {
		if idx := strings.Index(req.ID, "/"); idx > 0 {
			provider = req.ID[:idx]
		} else {
			provider = "custom"
		}
	}

	p := loadPool()
	poolMu.Lock()
	p.Models = append(p.Models, Model{
		ID:       req.ID,
		Provider: provider,
		Cost:     cost,
		Status:   "active",
		Custom:   true,
	})
	poolMu.Unlock()
	savePool()

		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "model_added")})
}

// POST /admin/api/models/delete  body: { id }
func handleAdminModelDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

		if req.ID == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "model_id_required")})
			return
		}

		p := loadPool()
		poolMu.Lock()
		found := false
		for i, m := range p.Models {
			if m.ID == req.ID {
				// 仅允许删除自定义模型
				if !m.Custom {
					poolMu.Unlock()
					writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "cannot_delete_builtin")})
					return
				}
				p.Models = append(p.Models[:i], p.Models[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			poolMu.Unlock()
			writeAPI(w, http.StatusNotFound, apiResponse{Error: tAPI(r, "model_not_found")})
			return
		}
	// 若删除的是当前默认模型，则清空回退到内置默认
	if p.DefaultModel == req.ID {
		p.DefaultModel = ""
	}
	pruneOrphanModelCooldownsLocked() // P3-14：删模型后清理其残留冷却项
	poolMu.Unlock()
	savePool()

		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "model_deleted")})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}

	p := loadPool()
	active, cooldown, expired := 0, 0, 0
	var usageCount, promptTokens, completionTokens, totalTokens, cachedTokens int64
	// P5-9：账号字段读收进 poolMu 快照（与选择器/用量自增的锁内写并发时，
	// 无锁遍历是数据竞争）；组装仍在锁外
	poolMu.Lock()
	for _, a := range p.Accounts {
		usageCount += a.UsageCount
		promptTokens += a.PromptTokens
		completionTokens += a.CompletionTokens
		totalTokens += a.TotalTokens
		cachedTokens += a.CachedTokens
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}
	total := len(p.Accounts)
	poolMu.Unlock()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":            total,
			"active":           active,
			"cooldown":         cooldown,
			"expired":          expired,
			"usageCount":       usageCount,
			"promptTokens":     promptTokens,
			"completionTokens": completionTokens,
			"totalTokens":      totalTokens,
			"cachedTokens":     cachedTokens,
			"strategy":         getProxyConfig().Strategy,
			"version":          appVersion,
			// opencode zen 免费模型今日用量（从请求日志聚合）
			"opencodeToday": opencodeUsageToday(),
		},
	})
}

// GET /admin/api/request-logs?limit=50&cursor=...
func handleAdminRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}

	limit := requestLogDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v) // P3-13：严格解析，"50abc" 类尾部垃圾整体拒绝
		if err != nil || n <= 0 {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_limit")})
			return
		}
		limit = n
	}
	cursor := r.URL.Query().Get("cursor")

	page, err := listRequestLogs(limit, cursor)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: page})
}

// GET /admin/api/opencode/config — opencode zen 配置 + 运行状态
func handleOpenCodeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	cfg := getZenConfig()
	maskedProxies := make([]string, 0, len(cfg.Proxies))
	for _, p := range cfg.Proxies {
		maskedProxies = append(maskedProxies, maskProxyURL(p))
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"enabled":         cfg.Enabled,
		"key":             cfg.Key,
		"baseURL":         cfg.BaseURL,
		"proxies":         maskedProxies,
		"proxyStrategy":   cfg.ProxyStrategy,
		"proxyCooldowns":  zenProxyCooldownStatus(),
		"maxConcurrency":  cfg.MaxConcurrency,
		"retries":         cfg.Retries,
		"failover":        cfg.Failover,
		"failoverCount":   cfg.FailoverCount,
		"failoverMinutes": cfg.FailoverMinutes,
		"compaction":      cfg.Compaction,
		"runtime": map[string]any{
			"failoverActive": zenFailedNow(),
		},
		"syncedModels": len(currentZenModels()),
		"lastSync":     lastZenModelSync(),
	}})
}

// POST /admin/api/opencode/config/update — 更新 opencode zen 配置（指针式补丁）
func handleOpenCodeConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()

	var req struct {
		Enabled         *bool             `json:"enabled"`
		Key             *string           `json:"key"`
		BaseURL         *string           `json:"baseURL"`
		Proxies         []string          `json:"proxies"`
		ProxyStrategy   *string           `json:"proxyStrategy"`
		MaxConcurrency  *int              `json:"maxConcurrency"`
		Retries         *int              `json:"retries"`
		Failover        *bool             `json:"failover"`
		FailoverCount   *int              `json:"failoverCount"`
		FailoverMinutes *int              `json:"failoverMinutes"`
		Compaction      *zenCompactConfig `json:"compaction"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}

	cfg := cloneZenConfig()
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.Key != nil {
		cfg.Key = strings.TrimSpace(*req.Key)
	}
	if req.BaseURL != nil {
		u := strings.TrimSpace(*req.BaseURL)
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_base_url")})
			return
		}
		cfg.BaseURL = u
	}
	if req.Proxies != nil {
		if err := validateProxyList(req.Proxies); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		var cleaned []string
		for _, p := range req.Proxies {
			if line := strings.TrimSpace(p); line != "" {
				cleaned = append(cleaned, line)
			}
		}
		cfg.Proxies = cleaned
	}
	if req.ProxyStrategy != nil {
		switch *req.ProxyStrategy {
		case "round_robin", "random", "fill":
			cfg.ProxyStrategy = *req.ProxyStrategy
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_proxy_strategy")})
			return
		}
	}
	if req.MaxConcurrency != nil {
		if *req.MaxConcurrency < 1 || *req.MaxConcurrency > 64 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_concurrency")})
			return
		}
		cfg.MaxConcurrency = *req.MaxConcurrency
	}
	if req.Retries != nil {
		if *req.Retries < 0 || *req.Retries > 10 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_retries")})
			return
		}
		cfg.Retries = *req.Retries
	}
	if req.Failover != nil {
		cfg.Failover = *req.Failover
	}
	if req.FailoverCount != nil {
		if *req.FailoverCount < 1 || *req.FailoverCount > 20 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_failover")})
			return
		}
		cfg.FailoverCount = *req.FailoverCount
	}
	if req.FailoverMinutes != nil {
		if *req.FailoverMinutes < 1 || *req.FailoverMinutes > 120 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_failover")})
			return
		}
		cfg.FailoverMinutes = *req.FailoverMinutes
	}
	if req.Compaction != nil {
		c := req.Compaction
		if c.Buffer < 0 || c.KeepTokens < 0 || c.MaxSummary < 0 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_compaction")})
			return
		}
		cfg.Compaction.Auto = c.Auto
		cfg.Compaction.Buffer = c.Buffer
		cfg.Compaction.KeepTokens = c.KeepTokens
		cfg.Compaction.SummaryModel = strings.TrimSpace(c.SummaryModel)
		cfg.Compaction.MaxSummary = c.MaxSummary
	}

	setZenConfig(cfg)
	log.Printf("admin: opencode config updated (enabled=%v)", cfg.Enabled)
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: tAPI(r, "opencode_config_saved")})
}

// POST /admin/api/opencode/models/sync — 手动触发一次 opencode 模型同步
func handleOpenCodeModelSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	res := syncZenModels()
	setLastZenModelSync(res)
	if res.Error != "" {
		writeAPI(w, http.StatusBadGateway, apiResponse{Success: false, Error: res.Error, Message: tAPI(r, "model_sync_failed")})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: res, Message: tAPI(r, "model_sync_done")})
}
