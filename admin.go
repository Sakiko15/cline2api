package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// 管理后台登录会话（内存态，程序重启后需重新登录）。
var (
	adminSessions   = make(map[string]time.Time)
	adminSessionsMu sync.Mutex
)

const (
	adminSessionCookie = "cline_admin_session"
	adminSessionTTL    = 24 * time.Hour
)

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
	mux.HandleFunc("/admin/api/models/add", auth(handleAdminModelAdd))
	mux.HandleFunc("/admin/api/models/delete", auth(handleAdminModelDelete))
	mux.HandleFunc("/admin/api/config", auth(handleAdminConfig))
	mux.HandleFunc("/admin/api/config/update", auth(handleAdminUpdateConfig))
	mux.HandleFunc("/admin/api/password", auth(handleAdminPassword))
	mux.HandleFunc("/admin/api/request-logs", auth(handleAdminRequestLogs))
	mux.HandleFunc("/admin/api/open-external", auth(handleOpenExternal))
}

// requireAdminAuth 后台访问鉴权中间件：未设置密码直接放行，否则校验会话 cookie。
func requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if loadPool().AdminPasswordHash == "" {
			next(w, r)
			return
		}
		c, err := r.Cookie(adminSessionCookie)
		if err != nil {
			writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "需要登录"})
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
		writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "登录已过期，请重新登录"})
	}
}

// hashAdminPassword 生成加盐密码哈希：hex(sha256(salt+password))。
func hashAdminPassword(saltHex, password string) string {
	sum := sha256.Sum256([]byte(saltHex + password))
	return hex.EncodeToString(sum[:])
}

// setAdminPassword 设置/修改/清除后台密码（空 = 清除），并清空所有会话强制重新登录。
func setAdminPassword(password string) {
	p := loadPool()
	poolMu.Lock()
	if password == "" {
		p.AdminPasswordHash = ""
		p.AdminPasswordSalt = ""
	} else {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			salt = []byte(time.Now().Format("20060102150405"))
		}
		p.AdminPasswordSalt = hex.EncodeToString(salt)
		p.AdminPasswordHash = hashAdminPassword(p.AdminPasswordSalt, password)
	}
	poolMu.Unlock()
	savePool()
	adminSessionsMu.Lock()
	adminSessions = make(map[string]time.Time)
	adminSessionsMu.Unlock()
}

// verifyAdminPassword 校验后台密码（未设置密码时返回 false）。
func verifyAdminPassword(password string) bool {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	if p.AdminPasswordHash == "" {
		return false
	}
	return hashAdminPassword(p.AdminPasswordSalt, password) == p.AdminPasswordHash
}

// randomHex 生成 n 字节随机数的 hex 字符串。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// POST /admin/api/login  body: {password}
func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if loadPool().AdminPasswordHash == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "后台未启用密码"})
		return
	}
	if !verifyAdminPassword(req.Password) {
		time.Sleep(500 * time.Millisecond) // 防爆破
		writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "密码错误"})
		return
	}
	token := randomHex(32)
	adminSessionsMu.Lock()
	adminSessions[token] = time.Now().Add(adminSessionTTL)
	adminSessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(adminSessionTTL.Seconds()),
	})
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "登录成功"})
}

// POST /admin/api/logout
func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminSessionCookie); err == nil {
		adminSessionsMu.Lock()
		delete(adminSessions, c.Value)
		adminSessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: "", Path: "/admin", MaxAge: -1})
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "已退出登录"})
}

// POST /admin/api/password  body: {password}（空 = 清除密码，恢复无密码访问）
func handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	setAdminPassword(req.Password)
	if req.Password == "" {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "已清除后台密码"})
	} else {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "后台密码已更新"})
	}
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
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
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
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
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "refreshToken is required"})
		return
	}

	// Validate by refreshing
	resp, err := refreshClineToken(req.RefreshToken)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid refreshToken: " + err.Error()})
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
	log.Printf("Account added via API: %s", req.Email)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Account %s added", req.Email),
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
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	if removeAccount(req.AccountID) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account deleted"})
	} else {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "Account not found"})
	}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
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
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	go func() {
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
		log.Printf("OAuth account added: %s", email)
	}()

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
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "sessionId required"})
		return
	}

	oauthSessionsMu.Lock()
	state, ok := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
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
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ssoCookies is required"})
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
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data:    result,
	})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
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
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if len(req.Tokens) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "tokens array is empty"})
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
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// GET /admin/api/accounts/export — 导出账号为批量导入兼容格式
func handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	p := loadPool()
	type exportToken struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	tokens := make([]exportToken, 0, len(p.Accounts))
	for _, acc := range p.Accounts {
		if acc.RefreshToken != "" {
			tokens = append(tokens, exportToken{
				RefreshToken: acc.RefreshToken,
				Email:        acc.Email,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cline-accounts-export.json"`)
	json.NewEncoder(w).Encode(map[string]any{
		"tokens":     tokens,
		"exportedAt": time.Now().Format(time.RFC3339),
	})
}

// GET /admin/api/open-external?url=... — 用系统默认浏览器打开外部链接
func handleOpenExternal(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("url")
	if url == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "url required"})
		return
	}
	// 仅允许 http/https，防止任意命令执行
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "only http/https URLs allowed"})
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
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for _, a := range p.Accounts {
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", a.Email, err)
		}
	}
	poolMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All tokens refreshed"})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All accounts deleted"})
}

// POST /admin/api/accounts/reset  body: { accountId }
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	// Reset status to active and refresh token, but preserve usage/token statistics.
	acc.Status = "active"
	if err := refreshAccountToken(acc); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "reset failed: " + err.Error()})
		return
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account reset"})
}

// POST /admin/api/accounts/test  body: { accountId?: "" }
func handleAdminAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	_ = json.Unmarshal(body, &req)

	p := loadPool()
	var targets []*Account
	if req.AccountID != "" {
		acc := getAccountByID(req.AccountID)
		if acc == nil {
			writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
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

// Global proxy config (mutable via API)
var (
	proxyConfig   = defaultProxyConfig()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
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

func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	proxyConfig = c
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	p := loadPool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": p.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	key := fmt.Sprintf("cline_%x_%x", time.Now().UnixMilli(), time.Now().UnixNano()%1000000)
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
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
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
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":      fmt.Sprintf("%s:%d", effectiveAdminHost(listenHost), listenPort),
		"host":         listenHost,
		"strategy":     cfg.Strategy,
		"version":      appVersion,
		"poolPath":     poolPath,
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
		"localIPs":     detectLocalIPs(),
		"hasPassword":  loadPool().AdminPasswordHash != "",
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel?, host? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy     string            `json:"strategy"`
		Headers      map[string]string `json:"headers"`
		DefaultModel string            `json:"defaultModel"`
		Host         string            `json:"host"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	cfg := getProxyConfig()
	changed := false
	restarting := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			cfg.Strategy = req.Strategy
			changed = true
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.Headers != nil {
		for k, v := range req.Headers {
			cfg.Headers[k] = v
		}
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
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid default model, not in available models list"})
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
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid host, must be 127.0.0.1, 0.0.0.0 or a local IP"})
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
		// 异步重启监听（Shutdown 会等待当前请求完成，不能在 handler 内同步调用）
		go func() {
			if err := restartListener(req.Host, listenPort); err != nil && err != http.ErrServerClosed {
				log.Printf("Listener restart failed: %v", err)
			}
		}()
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":      cfg.Strategy,
		"headers":       cfg.Headers,
		"defaultModel":  getDefaultModel(),
		"host":          listenHost,
		"address":       fmt.Sprintf("%s:%d", effectiveAdminHost(listenHost), listenPort),
		"restarting":    restarting,
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	models := getAllModels()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": models}})
}

// POST /admin/api/models/add  body: { id, provider?, cost? }
func handleAdminModelAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Cost     string `json:"cost"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.ID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "model id is required"})
		return
	}

	// 校验不与已有模型重复
	for _, m := range getAllModels() {
		if m.ID == req.ID {
			writeAPI(w, http.StatusConflict, apiResponse{Error: "model already exists"})
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

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model added"})
}

// POST /admin/api/models/delete  body: { id }
func handleAdminModelDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.ID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "model id is required"})
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
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: "cannot delete builtin model"})
				return
			}
			p.Models = append(p.Models[:i], p.Models[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		poolMu.Unlock()
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "model not found"})
		return
	}
	// 若删除的是当前默认模型，则清空回退到内置默认
	if p.DefaultModel == req.ID {
		p.DefaultModel = ""
	}
	poolMu.Unlock()
	savePool()

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model deleted"})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	p := loadPool()
	active, cooldown, expired := 0, 0, 0
	var usageCount, promptTokens, completionTokens, totalTokens, cachedTokens int64
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

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":            len(p.Accounts),
			"active":           active,
			"cooldown":         cooldown,
			"expired":          expired,
			"usageCount":       usageCount,
			"promptTokens":     promptTokens,
			"completionTokens": completionTokens,
			"totalTokens":      totalTokens,
			"cachedTokens":     cachedTokens,
			"strategy":         "round_robin",
			"version":          appVersion,
		},
	})
}

// GET /admin/api/request-logs?limit=50&cursor=...
func handleAdminRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	limit := requestLogDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid limit"})
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
