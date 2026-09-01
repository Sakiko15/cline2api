package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"
	fallbackDefaultModel   = "z-ai/glm-5.3-flash"
	freeModelPrimary       = "z-ai/glm-5.3-flash"
	freeModelFallback      = "deepseek/deepseek-v4-flash"
	freeModelLastResort    = "cline-free/longcat-2.0"
)

// freeModelChain 是 model="free" 时的降级顺序。
// 顺序依据 Artificial Analysis Intelligence Index v4.1.1：
// glm-5.3-flash 57 > deepseek-v4-flash 0731 52 > longcat-2.0 34。
var freeModelChain = []string{freeModelPrimary, freeModelFallback, freeModelLastResort}

// builtinModels 是内置默认模型列表（不可删除），仅作为离线 / 未同步时的 fallback。
// 同步 Cline 官方推荐模型成功后，getAllModels 以远程模型为主。
var builtinModels = []Model{
	{ID: "z-ai/glm-5.3-flash", Provider: "z-ai", Cost: "free", Status: "active", Custom: false},
	{ID: "cline-free/longcat-2.0", Provider: "cline-free", Cost: "free", Status: "active", Custom: false},
	{ID: "cline-pass/glm-5.2", Provider: "zai", Cost: "pass", Status: "active", Custom: false},
	{ID: "cline-pass/deepseek-v4-flash", Provider: "deepseek", Cost: "pass", Status: "active", Custom: false},
	{ID: "cline-pass/qwen3.7-max", Provider: "qwen", Cost: "pass", Status: "active", Custom: false},
	{ID: "deepseek/deepseek-v4-flash", Provider: "deepseek", Cost: "free", Status: "active", Custom: false},
	{ID: "poolside/laguna-s-2.1:free", Provider: "poolside", Cost: "free", Status: "active", Custom: false},
}

// getAllModels 返回可用模型列表：
//   - 已同步远程模型：Cline 远程（Source=remote）+ opencode 同步（Source=zen）+ 用户自定义
//   - 未同步 / 离线：内置 fallback（Cline + zen 种子表）+ 用户自定义
func getAllModels() []Model {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	var custom []Model
	var remote []Model
	var zen []Model
	for _, m := range p.Models {
		switch m.Source {
		case "remote":
			remote = append(remote, m)
		case "zen":
			zen = append(zen, m)
		default:
			custom = append(custom, m)
		}
	}

	if len(remote) > 0 || len(zen) > 0 || remoteZenActive() {
		result := make([]Model, 0, len(remote)+len(zen)+len(custom))
		result = append(result, remote...)
		result = append(result, zen...)
		result = append(result, custom...)
		return result
	}

	builtin := make([]Model, 0, len(builtinModels)+len(zenSeedModels))
	builtin = append(builtin, builtinModels...)
	builtin = append(builtin, builtinZenModels()...)

	result := make([]Model, 0, len(builtin)+len(custom))
	result = append(result, builtin...)
	result = append(result, custom...)
	return result
}

// getDefaultModel 返回用户设置的默认模型；未设置时优先回退到第一个远程 free 模型，
// 否则用内置 fallback。
func getDefaultModel() string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	if p.DefaultModel != "" {
		return p.DefaultModel
	}

	for _, m := range p.Models {
		if m.Source == "remote" && m.Cost == "free" {
			return m.ID
		}
	}

	for _, m := range builtinModels {
		if m.Cost == "free" {
			return m.ID
		}
	}
	return fallbackDefaultModel
}

// 当前监听地址（startProxy 启动时赋值，供管理后台展示）。
var (
	listenHost string
	listenPort int
)

// HTTP server 实例与路由表（restartListener 换地址重启时复用）。
var (
	serverMux     *http.ServeMux
	currentServer *http.Server
	serverMu      sync.Mutex
)

// restartListener 用新地址重启 HTTP 监听。
// 注意：必须在 goroutine 中调用——Shutdown 会等待当前 HTTP 请求完成，
// 若在 admin handler 内同步调用会死锁。
func restartListener(host string, port int) error {
	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	serverMu.Lock()
	old := currentServer
	serverMu.Unlock()

	shutdownOld := func() {
		if old == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.Shutdown(ctx)
		cancel()
	}

	// 先绑定新地址：目标端口被无关进程占用时 bind 失败直接返回，旧监听不受影响
	// （P1-9：旧实现先 Shutdown 再 ListenAndServe，绑定失败会导致零监听、全代理下线）
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 同端口换地址等场景：端口仍被旧监听自身占用 → 停旧后重试。
		// Windows 上 Shutdown 关闭 listener 后 socket 释放是异步的，立即重绑
		// 仍可能 EADDRINUSE，故带短暂退避重试数次。
		shutdownOld()
		old = nil
		for i := 0; i < 10; i++ {
			time.Sleep(50 * time.Millisecond)
			ln, err = net.Listen("tcp", addr)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("bind %s: %w", addr, err)
		}
	}

	server := &http.Server{Addr: addr, Handler: serverMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	serverMu.Lock()
	currentServer = server
	serverMu.Unlock()
	listenHost = host
	listenPort = port

	// 绑定成功后才停旧监听（若上一步未停）
	shutdownOld()

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  Listener restarted: %s\n", addr)
	if !isLoopbackHost(host) {
		for _, ip := range detectLocalIPs() {
			fmt.Printf("  http://%s:%d (LAN)\n", ip, port)
		}
		fmt.Println("  !!! 监听非本机地址，管理后台无鉴权，请确认网络环境安全")
	}
	fmt.Println(strings.Repeat("=", 58))
	return server.Serve(ln)
}

// effectiveAdminHost 返回管理后台/浏览器实际可用的访问地址：
// host 为空或通配地址（0.0.0.0 / ::）时展示回环 127.0.0.1，否则返回 host 本身。
func effectiveAdminHost(host string) string {
	switch host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return host
}

// detectLocalIPs 检测本机所有可用 IPv4 地址（排除回环、链路本地和未启用的网卡）。
func detectLocalIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			result = append(result, v4.String())
		}
	}
	return result
}

// isLoopbackHost 判断监听地址是否为回环（127.x / localhost），用于安全提示。
func isLoopbackHost(host string) bool {
	switch host {
	case "", "localhost", "127.0.0.1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata",
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ReasoningEffortAlt  string          `json:"reasoningEffort,omitempty"`
	Extra               map[string]any  `json:"-"`
}

func startProxy(host string, port int) error {
	p := loadPool()
	loadRequestLogs()
	activeCount := 0
	for _, a := range p.Accounts {
		if a.Status == "active" {
			// Try to pre-warm tokens
			if a.AccessToken == "" || time.Now().UnixMilli() >= a.ExpiresAt {
				if err := refreshAccountToken(a); err != nil {
					log.Printf("  Pre-warm failed for %s: %v", a.Email, err)
					continue
				}
			}
			activeCount++
		}
	}
	log.Printf("Loaded %d active accounts from pool", activeCount)

	// 启动时异步同步一次 Cline 官方推荐模型（不阻塞启动）
	startModelSync()

	// opencode zen：定时同步免费模型列表 + 压缩会话状态清理
	if getZenConfig().Enabled {
		startZenModelsRefresher()
	}
	startCompactCleanup()

	freePort(port)

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		info := map[string]any{
			"status":         "ok",
			"version":        appVersion,
			"activeAccounts": activeCount,
		}
		writeJSON(w, http.StatusOK, info)
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"version":        appVersion,
			"activeAccounts": activeCount,
		})
	}))

	// Admin API (frontend + REST)
	// CLINE_ADMIN_PASSWORD：未设密码时的一次性引导（配合管理面 fail-closed，
	// 公网实例无需本机登录即可设置初始密码；已设密码的实例忽略该变量）
	if envPwd := os.Getenv("CLINE_ADMIN_PASSWORD"); envPwd != "" && loadPool().AdminPasswordHash == "" {
		setAdminPassword(envPwd)
		log.Printf("admin password bootstrapped from CLINE_ADMIN_PASSWORD environment variable")
	}

	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			p := loadPool()
			if len(p.Keys) == 0 {
				next(w, r)
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			valid := false
			for _, k := range p.Keys {
				if k == key {
					valid = true
					break
				}
			}

			if !valid {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]string{
						"message": "invalid API key. Generate one at /admin/ or set x-api-key header",
						"type":    "auth_error",
					},
				})
				return
			}
			next(w, r)
		})
	}

	modelsHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		all := getAllModels()
		list := make([]map[string]any, len(all))
		for i, m := range all {
			ownedBy := "cline"
			if m.Source == "zen" || m.Provider == "opencode" {
				ownedBy = "opencode"
			}
			list[i] = map[string]any{
				"id":       m.ID,
				"object":   "model",
				"created":  time.Now().UnixMilli(),
				"owned_by": ownedBy,
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
	})
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/models", modelsHandler)

	chatHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if activeCount == 0 && len(loadPool().Accounts) == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"message": "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.",
					"type":    "auth_error",
				},
			})
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		isStream, _ := params["stream"].(bool)
		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		model, _ := params["model"].(string)
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, model)

		reqLog := RequestLog{StartedAt: time.Now(), Protocol: "openai", Model: model, Stream: isStream}

		// Override system prompt from override.md for OpenAI format
		if override := loadOverrideContent(); override != "" {
			if msgs, ok := params["messages"].([]any); ok {
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if mm["role"] == "system" {
							mm["content"] = override
							found = true
							break
						}
					}
				}
				if !found {
					params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
				}
			}
		}

		// 按 model 自动分流：zen 免费模型 / zen 付费拒绝 / 其余走 Cline 池
		switch routeModel(model) {
		case "reject":
			msg := fmt.Sprintf("model %q is a paid opencode model; only free models are proxied", model)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": msg, "type": "invalid_request_error"},
			})
			return
		case "zen":
			reqLog.Upstream = upstreamOpenCode
			zm, _ := resolveZenInfo(model)
			out := maybeCompact(r.Context(), params, zm, requestSessionID(params, r.Header))
			if out.changed {
				log.Printf("  chat %s", out.note)
			}
			resp, err := callZenAPI(r.Context(), params, isStream)
			if err != nil {
				log.Printf("  api error: %v", err)
				finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
				writeUpstreamError(w, err)
				return
			}
			defer resp.Body.Close()
			if isStream {
				handleStreamResponse(w, resp, nil, &reqLog)
			} else {
				handleNonStreamResponse(w, resp, nil, &reqLog)
			}
			return
		}

		if activeCount == 0 && len(loadPool().Accounts) == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"message": "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.",
					"type":    "auth_error",
				},
			})
			return
		}

		resp, acc, err := callClineAPI(r.Context(), params, isStream)
		if effectiveModel, ok := params["model"].(string); ok && effectiveModel != "" {
			reqLog.Model = effectiveModel
		}
		if err != nil {
			log.Printf("  api error: %v", err)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
			writeUpstreamError(w, err)
			return
		}
		reqLog.Upstream = upstreamCline
		defer resp.Body.Close()
		if acc != nil {
			reqLog.AccountID = acc.AccountID
			reqLog.AccountEmail = acc.Email
		}

		if isStream {
			handleStreamResponse(w, resp, acc, &reqLog)
		} else {
			handleNonStreamResponse(w, resp, acc, &reqLog)
		}
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/chat/completions", chatHandler)

	// Anthropic Messages API support
	anthropicHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handleAnthropicMessages(w, r)
	})
	mux.HandleFunc("/v1/messages", anthropicHandler)
	mux.HandleFunc("/messages", anthropicHandler)

	// OpenAI Responses API support（所有上游：zen 免费模型 + Cline 账号池）
	responsesHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handleResponses(w, r)
	})
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)

	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	listenHost = host
	listenPort = port
	serverMux = mux
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,  // slowloris 防护（P1-3）
		IdleTimeout:       120 * time.Second, // WriteTimeout 留 0，SSE 长流不受影响
	}
	serverMu.Lock()
	currentServer = server
	serverMu.Unlock()

	// 启动后台冷却恢复巡检
	startCooldownRecovery()

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  Cline Go Proxy %s - No CLI Required\n", appVersion)
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  http://%s\n", addr)
	fmt.Printf("  http://%s/v1\n", addr)
	if !isLoopbackHost(host) {
		for _, ip := range detectLocalIPs() {
			fmt.Printf("  http://%s:%d (LAN)\n", ip, port)
		}
		fmt.Println("  !!! 监听非本机地址，管理后台无鉴权，请确认网络环境安全")
	}
	fmt.Println("  API Key: any value")
	fmt.Printf("  Model:   %s\n", getDefaultModel())
	fmt.Printf("  Accounts: %d total, %d active\n", len(loadPool().Accounts), activeCount)
	if zc := getZenConfig(); zc.Enabled {
		fmt.Printf("  OpenCode: enabled (%s free models)\n", strings.TrimRight(zc.BaseURL, "/"))
	} else {
		fmt.Println("  OpenCode: disabled")
	}
	fmt.Println(strings.Repeat("=", 58))

	return server.ListenAndServe()
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

// clampMaxTokens 防御数值边界：非正数或超出转换安全范围的浮点（int 转换会溢出为负）
// 一律回落默认值（P1-12）。
func clampMaxTokens(v float64) int {
	if v <= 0 || v > 1e9 {
		return defaultMaxTokens
	}
	return int(v)
}

func buildUpstreamBody(params map[string]any, stream bool) map[string]any {
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := defaultMaxTokens
	if mt, ok := params["max_tokens"].(float64); ok {
		maxTokens = clampMaxTokens(mt)
	} else if mt, ok := params["max_completion_tokens"].(float64); ok {
		maxTokens = clampMaxTokens(mt)
	}

	model := getDefaultModel()
	if m, ok := params["model"].(string); ok && m != "" {
		model = m
	}

	body := map[string]any{
		"model":            model,
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	return body
}

func clineHeaders(token, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", sessionID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

type clineAPIError struct {
	statusCode int
	message    string
}

func (e *clineAPIError) Error() string {
	return fmt.Sprintf("API %d: %s", e.statusCode, e.message)
}

type clineAccountUnavailableError struct {
	err error
}

func (e *clineAccountUnavailableError) Error() string {
	return e.err.Error()
}

func (e *clineAccountUnavailableError) Unwrap() error {
	return e.err
}

type freeModelUnavailableError struct {
	message string
}

func (e *freeModelUnavailableError) Error() string {
	return e.message
}

// upstreamErrorHTTPStatus 将上游错误映射为客户端状态码（P2-9：此前除 free 链
// 耗尽 429 外全部坍缩为 500，客户端 SDK 把注定失败的请求当可重试 5xx 反复重试）：
//   - zen 上游状态 ≥400 原样透传，否则 502
//   - free 链耗尽（freeModelUnavailableError）仍 429
//   - cline 上游状态 ≥400 原样透传
//   - transport/取消/本地错误 → 500
func upstreamErrorHTTPStatus(err error) int {
	var zerr *zenAPIError
	if errors.As(err, &zerr) {
		if zerr.statusCode >= 400 && zerr.statusCode < 600 {
			return zerr.statusCode
		}
		return http.StatusBadGateway
	}
	if _, ok := err.(*freeModelUnavailableError); ok {
		return http.StatusTooManyRequests
	}
	var apiErr *clineAPIError
	if errors.As(err, &apiErr) && apiErr.statusCode >= 400 {
		return apiErr.statusCode
	}
	return http.StatusInternalServerError
}

// retryAfterFor 解析 429 的客户端重试提示：zen 用上游 Retry-After 头；
// cline 仅当错误文本含冷却时长（"Try again in Xh Ym"）时回填，钳制 1 小时。
func retryAfterFor(err error) time.Duration {
	var zerr *zenAPIError
	if errors.As(err, &zerr) {
		return clampRetryWait(zerr.retryAfter, time.Hour)
	}
	msg := err.Error()
	if !cooldownHoursRe.MatchString(msg) && !cooldownMinutesRe.MatchString(msg) {
		return 0
	}
	return clampRetryWait(time.Until(parseCooldownUntil(msg)), time.Hour)
}

// writeUpstreamError 统一 /v1 错误响应：按上游状态映射状态码，429 回填 Retry-After。
func writeUpstreamError(w http.ResponseWriter, err error) {
	status := upstreamErrorHTTPStatus(err)
	if status == http.StatusTooManyRequests {
		if d := retryAfterFor(err); d > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		}
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": err.Error(), "type": "api_error"},
	})
}

func callClineAPI(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	model, _ := params["model"].(string)
	if model == "free" {
		return callFreeClineAPI(ctx, params, stream)
	}

	acc := pickAccountForModel(model)
	if acc == nil {
		return nil, nil, fmt.Errorf("no active accounts available. Use --login or admin API to add accounts")
	}
	return callClineAPIWithAccount(ctx, acc, params, stream)
}

func callFreeClineAPI(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	for _, model := range freeModelChain {
		params["model"] = model
		for {
			acc := pickAccountForModelStrict(model)
			if acc == nil {
				break
			}

			resp, usedAcc, err := callClineAPIWithAccount(ctx, acc, params, stream)
			if err == nil {
				return resp, usedAcc, nil
			}
			var accountErr *clineAccountUnavailableError
			if errors.As(err, &accountErr) {
				continue
			}
			apiErr, ok := err.(*clineAPIError)
			if !ok {
				return nil, usedAcc, err
			}
			if apiErr.statusCode >= 500 {
				// 上游服务端错误：换模型大概率同样失败，中断整链（P1-11）
				return nil, usedAcc, err
			}
			// 4xx（含 429/400/404）：该模型/账号组合被上游拒绝 → 推进下一账号，
			// 链内账号耗尽后由外层循环尝试下一链模型（P1-11）
		}
	}
	return nil, nil, &freeModelUnavailableError{message: "no eligible accounts available for free models"}
}

func callClineAPIWithAccount(ctx context.Context, acc *Account, params map[string]any, stream bool) (*http.Response, *Account, error) {
	token, err := ensureAccountToken(acc)
	if err != nil {
		// Try other accounts
		return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("account %s token failed: %w", acc.Email, err)}
	}

	body := buildUpstreamBody(params, stream)
	sessionID, _ := body["session_id"].(string)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, acc, fmt.Errorf("marshal body: %w", err)
	}

	// 绑定请求上下文：客户端断开时上游请求随之取消（P1-4）
	req, err := http.NewRequestWithContext(ctx, "POST", clineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, acc, fmt.Errorf("create request: %w", err)
	}
	req.Header = clineHeaders(token, sessionID)

	toolCount := 0
	if tools, ok := params["tools"]; ok {
		if t, ok := tools.([]any); ok {
			toolCount = len(t)
		}
	}
	log.Printf("  upstream: account=%s stream=%v tools=%d msgs=%d max_tokens=%v effort=%v",
		truncateEmail(acc.Email), stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// 客户端断开/请求取消不是账号问题，不得冷却账号，
			// 否则 free 链会因一次用户取消把全池账号依次打入冷却（P1-4 回归修复）
			return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("upstream request canceled: %w", err)}
		}
		markAccountCooldown(acc, time.Now().Add(5*time.Minute))
		return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("upstream request: %w", err)}
	}

	if resp.StatusCode == 401 {
		resp.Body.Close()
		// Refresh token and retry
		if err := refreshAccountToken(acc); err == nil {
			token = acc.AccessToken
			req.Header = clineHeaders(token, sessionID)
			req.Body = io.NopCloser(bytes.NewReader(bodyJSON))
			resp, err = httpClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("upstream retry canceled: %w", err)}
				}
				markAccountCooldown(acc, time.Now().Add(5*time.Minute))
				return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("upstream retry: %w", err)}
			}
			if resp.StatusCode == 401 {
				resp.Body.Close()
				markAccountExpired(acc)
				return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("account %s token expired permanently", acc.Email)}
			}
		} else {
			// 仅上游明确拒绝（4xx）才置 expired；暂态失败保持账号可用（P2-6）
			var rej *tokenRefreshRejectedError
			if errors.As(err, &rej) {
				markAccountExpired(acc)
			}
			return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("account %s refresh failed: %w", acc.Email, err)}
		}
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyStr := string(bodyBytes)
		// 429：模型级冷却 —— 只暂停该模型，账号保持可用，其他模型继续转发
		if resp.StatusCode == 429 {
			model, _ := body["model"].(string)
			until := parseCooldownUntil(bodyStr)
			if model != "" {
				setModelCooldown(acc, model, until)
			} else {
				markAccountCooldown(acc, until)
			}
		}
		return nil, acc, &clineAPIError{statusCode: resp.StatusCode, message: truncate(bodyStr, 500)}
	}

	markAccountUsed(acc)
	return resp, acc, nil
}

type accountTestResult struct {
	AccountID    string `json:"accountId"`
	Email        string `json:"email"`
	OK           bool   `json:"ok"`
	DurationMs   int64  `json:"durationMs"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Error        string `json:"error,omitempty"`
}

// parseCooldownUntil 从 429 响应体中解析等待时长，支持 "1h 1m"、"2h"、"45m" 格式。
// 分钟-only 文本曾被误读为小时（45m→45h，P1-8）；结果钳制到 [1m, 24h]，
// 解析失败或 0h 0m 回退 1 小时。
var (
	cooldownHoursRe   = regexp.MustCompile(`(?i)try\s+again\s+in\s+(\d+)\s*h(?:\s*(\d+)\s*m?)?`)
	cooldownMinutesRe = regexp.MustCompile(`(?i)try\s+again\s+in\s+(\d+)\s*m\b`)
)

func parseCooldownUntil(body string) time.Time {
	var d time.Duration
	if m := cooldownHoursRe.FindStringSubmatch(body); m != nil {
		hours, _ := strconv.Atoi(m[1])
		if hours > 24 {
			hours = 24 // 预钳制，防止大数乘法让 time.Duration 溢出回绕
		}
		minutes := 0
		if len(m) >= 3 && m[2] != "" {
			minutes, _ = strconv.Atoi(m[2])
		}
		d = time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	} else if m := cooldownMinutesRe.FindStringSubmatch(body); m != nil {
		minutes, _ := strconv.Atoi(m[1])
		d = time.Duration(minutes) * time.Minute
	}
	if d <= 0 {
		d = time.Hour // 解析失败或 0h 0m
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return time.Now().Add(d)
}

// startCooldownRecovery 启动后台 goroutine，每 30 秒检查一次 cooldown 账号，
// 对 CooldownUntil 已过期的账号执行探活，成功则自动激活；
// 并每 ~5 分钟低频探活 expired 账号（P2-6：刷新 token 仍有效的误标账号可自愈）。
func startCooldownRecovery() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		tick := 0
		for range ticker.C {
			tick++
			p := loadPool()
			poolMu.Lock()
			var toRecover []*Account
			var toReactivate []*Account
			for _, acc := range p.Accounts {
				switch {
				case acc.Status == "cooldown":
					// 有恢复时间且已过期 → 探活
					// 无恢复时间（旧数据）→ 也尝试探活
					if acc.CooldownUntil.IsZero() || time.Now().After(acc.CooldownUntil) {
						toRecover = append(toRecover, acc)
					}
				case acc.Status == "expired" && tick%10 == 0:
					toReactivate = append(toReactivate, acc)
				}
			}
			poolMu.Unlock()

			for _, acc := range toRecover {
				log.Printf("cooldown recovery: testing %s", acc.Email)
				result := testAccount(acc)
				if result.OK {
					log.Printf("cooldown recovery: %s reactivated", acc.Email)
				} else {
					log.Printf("cooldown recovery: %s still unavailable: %s", acc.Email, result.Error)
				}
			}
			for _, acc := range toReactivate {
				log.Printf("expired account probe: testing %s", acc.Email)
				result := testAccount(acc)
				if result.OK {
					log.Printf("expired account probe: %s reactivated", acc.Email)
				} else {
					log.Printf("expired account probe: %s still unavailable: %s", acc.Email, result.Error)
				}
			}
		}
	}()
}

// testAccount sends a minimal "hi" request through a specific account to verify
// it can complete an upstream call. It does not update aggregate token counters
// or request logs; it is a diagnostic-only probe.
func testAccount(acc *Account) accountTestResult {
	result := accountTestResult{AccountID: acc.AccountID, Email: acc.Email}
	started := time.Now()

	params := map[string]any{
		"model":      getDefaultModel(),
		"max_tokens": 16,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	resp, _, err := callClineAPIWithAccount(context.Background(), acc, params, false)
	if err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = truncate(err.Error(), 200)
		return result
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = "read response: " + truncate(err.Error(), 200)
		return result
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = "decode response: " + truncate(err.Error(), 200)
		return result
	}
	if data, ok := obj["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			obj = d
		}
	}
	obj = normalizeOpenAIResponse(obj)
	usage := parseTokenUsage(obj["usage"])

	result.OK = true
	result.DurationMs = time.Since(started).Milliseconds()
	if usage.Valid {
		result.InputTokens = usage.Prompt
		result.OutputTokens = usage.Completion
	}
	// If the account was in cooldown/expired but the test succeeded, restore it.
	if acc.Status != "active" {
		markAccountActive(acc)
	}
	return result
}

type tokenUsage struct {
	Prompt     int64
	Completion int64
	Total      int64
	Cached     int64
	Valid      bool
}

func parseTokenUsage(value any) tokenUsage {
	usage, ok := value.(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	read := func(keys ...string) int64 {
		for _, key := range keys {
			// 上限 1e15：超过该量级的浮点转 int64 会溢出为负（P1-12）
			if value, ok := usage[key].(float64); ok && value >= 0 && value <= 1e15 {
				return int64(value)
			}
		}
		return 0
	}
	readNested := func(parent string, keys ...string) int64 {
		details, ok := usage[parent].(map[string]any)
		if !ok {
			return 0
		}
		for _, key := range keys {
			if value, ok := details[key].(float64); ok && value >= 0 {
				return int64(value)
			}
		}
		return 0
	}
	prompt := read("prompt_tokens", "input_tokens")
	completion := read("completion_tokens", "output_tokens")
	cached := int64(0)
	if nested := readNested("prompt_tokens_details", "cached_tokens"); nested > 0 {
		cached = nested
	} else if nested := readNested("input_tokens_details", "cached_tokens"); nested > 0 {
		cached = nested
	} else if v := read("cache_read_input_tokens") + read("cache_creation_input_tokens"); v > 0 {
		cached = v
	} else {
		cached = read("prompt_cache_hit_tokens", "prompt_cache_creation_tokens", "cached_tokens")
	}
	total := read("total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	_, hasUsage := usage["prompt_tokens"]
	if !hasUsage {
		_, hasUsage = usage["input_tokens"]
		if !hasUsage {
			if _, hasUsage = usage["completion_tokens"]; !hasUsage {
				if _, hasUsage = usage["output_tokens"]; !hasUsage {
					_, hasUsage = usage["total_tokens"]
				}
			}
		}
	}
	if !hasUsage {
		_, hasUsage = usage["cache_read_input_tokens"]
		if !hasUsage {
			_, hasUsage = usage["cache_creation_input_tokens"]
			if !hasUsage {
				_, hasUsage = usage["prompt_tokens_details"]
				if !hasUsage {
					_, hasUsage = usage["input_tokens_details"]
				}
			}
		}
	}
	return tokenUsage{Prompt: prompt, Completion: completion, Total: total, Cached: cached, Valid: hasUsage}
}

func mergeTokenUsage(current, next tokenUsage) tokenUsage {
	if !next.Valid {
		return current
	}
	if next.Prompt != 0 {
		current.Prompt = next.Prompt
	}
	if next.Completion != 0 {
		current.Completion = next.Completion
	}
	if next.Total != 0 {
		current.Total = next.Total
	}
	if next.Cached != 0 {
		current.Cached = next.Cached
	}
	current.Valid = current.Valid || next.Valid
	if current.Total == 0 && (current.Prompt != 0 || current.Completion != 0) {
		current.Total = current.Prompt + current.Completion
	}
	return current
}

func recordTokenUsage(acc *Account, model string, usage tokenUsage) {
	if acc == nil || !usage.Valid {
		return
	}
	// 先判断是否免费模型（getAllModels 会拿 poolMu，必须在持有锁之前计算）
	isFree := model != "" && isFreeModelID(model)
	poolMu.Lock()
	acc.PromptTokens += usage.Prompt
	acc.CompletionTokens += usage.Completion
	acc.TotalTokens += usage.Total
	acc.CachedTokens += usage.Cached
	// 按模型细分统计（仅记录 free 模型）
	if isFree {
		if acc.ModelStats == nil {
			acc.ModelStats = make(map[string]*ModelStat)
		}
		st := acc.ModelStats[model]
		if st == nil {
			st = &ModelStat{ModelID: model, Cost: "free"}
			acc.ModelStats[model] = st
		}
		st.UsageCount++
		st.PromptTokens += usage.Prompt
		st.CompletionTokens += usage.Completion
		st.TotalTokens += usage.Total
		st.CachedTokens += usage.Cached
	}
	poolMu.Unlock()
	savePool()
}

// isFreeModelID 判断模型是否为 free 计费（用于按模型统计和模型级冷却）。
func isFreeModelID(model string) bool {
	for _, m := range getAllModels() {
		if m.ID == model {
			return m.Cost == "free"
		}
	}
	// 未知模型：按 ID 后缀/前缀启发式判断
	return strings.HasSuffix(model, ":free") || strings.Contains(model, "/free/")
}

// modelCooldownActive 判断某账号下该模型是否处于模型级冷却中。
func modelCooldownActive(acc *Account, model string) bool {
	if acc == nil || model == "" {
		return false
	}
	poolMu.Lock()
	defer poolMu.Unlock()
	until, ok := acc.ModelCooldowns[model]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(acc.ModelCooldowns, model)
		savePoolLocked()
		return false
	}
	return true
}

// setModelCooldown 记录模型级冷却（429 时调用）：只暂停该模型，账号保持可用。
// fallback 为解析失败时的恢复时长（默认 1 小时）。
func setModelCooldown(acc *Account, model string, until time.Time) {
	if acc == nil || model == "" {
		return
	}
	poolMu.Lock()
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = make(map[string]time.Time)
	}
	acc.ModelCooldowns[model] = until
	poolMu.Unlock()
	savePool()
	log.Printf("model cooldown: account=%s model=%s until=%s", truncateEmail(acc.Email), model, until.Format("15:04:05"))
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}

func handleStreamResponse(w http.ResponseWriter, upstream *http.Response, acc *Account, reqLog *RequestLog) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return
	}

	// 写失败即中止：客户端断开/停滞时停止消费上游（P1-4）
	writeChunk := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			log.Printf("  stream write failed (client gone?): %v", err)
			return false
		}
		flusher.Flush()
		return true
	}

	reader := bufio.NewReader(upstream.Body)
	var latestUsage tokenUsage
	var firstOutputAt time.Time
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if line != "" {
					writeChunk([]byte(line + "\n"))
				}
			}
			break
		}

		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" || payload == "[DONE]" {
				if !writeChunk([]byte(line + "\n\n")) {
					break
				}
				continue
			}

			// Try to normalize the response
			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err == nil {
				// Some Cline responses wrap in {data: {...}}
				if data, ok := obj["data"]; ok {
					if d, ok := data.(map[string]any); ok {
						if _, hasChoices := d["choices"]; hasChoices {
							obj = d
						}
						if _, hasID := d["id"]; hasID {
							obj = d
						}
					}
				}
				normalized := normalizeOpenAIResponse(obj)
				if usage := parseTokenUsage(normalized["usage"]); usage.Valid {
					latestUsage = mergeTokenUsage(latestUsage, usage)
				}
				if firstOutputAt.IsZero() && hasFirstOutput(normalized) {
					firstOutputAt = time.Now()
				}
				if normBytes, err := json.Marshal(normalized); err == nil {
					if !writeChunk([]byte("data: " + string(normBytes) + "\n\n")) {
						break
					}
					continue
				}
			}
		}

		if !writeChunk([]byte(line + "\n")) {
			break
		}
	}
	recordTokenUsage(acc, reqLog.Model, latestUsage)
	finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, true, "")
}

func hasFirstOutput(obj map[string]any) bool {
	choices, ok := getNested(obj, "choices").([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return false
	}
	if delta, ok := choice["delta"].(map[string]any); ok {
		if c, _ := delta["content"].(string); c != "" {
			return true
		}
		if tc, ok := delta["tool_calls"].([]any); ok && len(tc) > 0 {
			return true
		}
	}
	if msg, ok := choice["message"].(map[string]any); ok {
		if c, _ := msg["content"].(string); c != "" {
			return true
		}
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			return true
		}
	}
	return false
}

func handleNonStreamResponse(w http.ResponseWriter, upstream *http.Response, acc *Account, reqLog *RequestLog) {
	var raw map[string]any
	if err := json.NewDecoder(upstream.Body).Decode(&raw); err != nil {
		finalizeRequestLog(reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, "decode response: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)
	usage := parseTokenUsage(out["usage"])
	recordTokenUsage(acc, reqLog.Model, usage)
	finalizeRequestLog(reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
}

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type toolAccumulator struct {
	index    int    // Anthropic content_block index（content_block_start 时分配，不复用上游序号）
	id       string
	name     string
	args     string
	started  bool // content_block_start 已发出
	open     bool // start 已发、stop 未发
	closed   bool // 已 stop；Anthropic 块不可重开，迟到片段丢弃
	sentArgs int  // 已通过 input_json_delta 发出的 args 字节数
}

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"` // 指针区分「未设置」与显式 0（P1-13）
	TopP        *float64        `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Extra       map[string]any  `json:"-"`
}

func loadOverrideContent() string {
	data, err := os.ReadFile("override.md")
	if err != nil {
		log.Printf("  override.md not found: %v", err)
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content != "" {
		log.Printf("  using override.md as system prompt (%d bytes)", len(content))
	} else {
		log.Printf("  override.md is empty")
	}
	return content
}

func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// Convert Anthropic format to OpenAI
			oai := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tMap["name"],
					"description": tMap["description"],
					"parameters":  tMap["input_schema"],
				},
			}
			out = append(out, oai)
		}
	}
	return out
}

// mapAnthropicToolChoice 将 Anthropic 的 tool_choice 形状映射为 OpenAI 形状（P1-13）：
// {"type":"auto"}→"auto"，{"type":"any"}→"required"，{"type":"none"}→"none"，
// {"type":"tool","name":x}→{"type":"function","function":{"name":x}}。
func mapAnthropicToolChoice(raw json.RawMessage) any {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "auto"
	}
	switch tc.Type {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	default: // "auto" 及未知值
		return "auto"
	}
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	// Temperature/TopP 为指针：nil=未设置，显式 0 也要透传（P1-13）
	if req.Temperature != nil {
		openAI["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		openAI["top_p"] = *req.TopP
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	// stop_sequences → stop（P1-13：此前解析后从未转发）
	if len(req.Stop) > 0 {
		var stops []string
		if err := json.Unmarshal(req.Stop, &stops); err == nil && len(stops) > 0 {
			openAI["stop"] = stops
		}
	}
	if req.ToolChoice != nil {
		openAI["tool_choice"] = mapAnthropicToolChoice(req.ToolChoice)
	}

	msgs := []any{}

	// System prompt: use override.md if it exists, otherwise use Anthropic's system field
	sysContent := loadOverrideContent()
	if sysContent == "" && req.System != nil {
		sysContent = extractStringContent(req.System)
	}
	if sysContent != "" {
		log.Printf("  system prompt: %d bytes (from override.md)", len(sysContent))
		msgs = append(msgs, map[string]any{"role": "system", "content": sysContent})
	}

	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
		case []any:
			textParts := []string{}
			var toolCalls []any
			var toolResults []any // 并行工具调用可有多个 tool_result，全部保留（P1-13）

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// skip images
					case "tool_use":
						argsStr := "{}"
						if input, ok := b["input"]; ok && input != nil {
							if s, ok := input.(string); ok {
								argsStr = s
							} else if bts, err := json.Marshal(input); err == nil {
								argsStr = string(bts)
							}
						}
						tc := map[string]any{
							"id":   b["id"],
							"type": "function",
							"function": map[string]any{
								"name":      b["name"],
								"arguments": argsStr,
							},
						}
						toolCalls = append(toolCalls, tc)
					case "tool_result":
						toolResults = append(toolResults, map[string]any{
							"role":         "tool",
							"content":      b["content"],
							"tool_call_id": b["tool_use_id"],
						})
					}
				}
			}

			if m.Role == "assistant" && len(toolCalls) > 0 {
				msg := map[string]any{
					"role":       "assistant",
					"content":    strings.Join(textParts, "\n"),
					"tool_calls": toolCalls,
				}
				msgs = append(msgs, msg)
			} else if m.Role == "user" && len(toolResults) > 0 {
				msgs = append(msgs, toolResults...)
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	out := map[string]any{
		"id":    "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"type":  "message",
		"role":  "assistant",
		"model": getNested(openAI, "model"),
	}

	// 安全断言：{"choices":[]} / {"choices":[null]} 等异常上游响应不得 panic（P1-6）
	choice0, _ := getNested(openAI, "choices", 0).(map[string]any)
	if choice0 == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}

	text := ""
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{map[string]any{"type": "text", "text": text}}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			contentBlocks = []any{} // Clear text-only, proper response has both
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    tcMap["id"],
						"name":  funcData["name"],
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}

	out["content"] = contentBlocks

	stopReason := "end_turn"
	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	}
	// 上游报 stop 但实际给了 tool_calls（部分上游如此）：Anthropic 语义必须 tool_use
	if stopReason == "end_turn" {
		for _, b := range contentBlocks {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_use" {
				stopReason = "tool_use"
				break
			}
		}
	}
	out["stop_reason"] = stopReason

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "parse_error"},
		})
		return
	}

	if req.MaxTokens <= 0 {
		req.MaxTokens = defaultMaxTokens
	}

	openAIReq := anthropicToOpenAI(req)

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", req.Model, req.Stream, len(req.Messages))

	reqLog := RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: req.Model, Stream: req.Stream}

	// 按 model 自动分流（与 chat 端点一致）：zen 免费/付费拒绝/Cline 池
	switch routeModel(req.Model) {
	case "reject":
		msg := fmt.Sprintf("model %q is a paid opencode model; only free models are proxied", req.Model)
		finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": msg, "type": "invalid_request_error"},
		})
		return
	case "zen":
		reqLog.Upstream = upstreamOpenCode
		zm, _ := resolveZenInfo(req.Model)
		out := maybeCompact(r.Context(), openAIReq, zm, requestSessionID(map[string]any{"session_id": r.Header.Get("x-opencode-session")}, nil))
		if out.changed {
			log.Printf("  anthropic %s", out.note)
		}
		resp, err := callZenAPI(r.Context(), openAIReq, req.Stream)
		if err != nil {
			log.Printf("  anthropic api error: %v", err)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
			writeUpstreamError(w, err)
			return
		}
		defer resp.Body.Close()
		if req.Stream {
			handleAnthropicStream(w, resp, nil, &reqLog)
		} else {
			var raw map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
				finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, "decode response: "+err.Error())
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]string{"message": err.Error(), "type": "parse_error"},
				})
				return
			}
			out2 := normalizeOpenAIResponse(unwrapDataEnvelope(raw))
			usage := parseTokenUsage(out2["usage"])
			finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
			// openAIToAnthropic 已正确保留文本块并映射 stop_reason（P2-10：
			// 此前在此整体清空 content，"先说明再调工具"的输出文本全丢）
			anthropicResp := openAIToAnthropic(out2)
			writeJSON(w, http.StatusOK, anthropicResp)
		}
		return
	}

	activeCount := 0
	p := loadPool()
	for _, a := range p.Accounts {
		if a.Status == "active" {
			activeCount++
		}
	}

	if activeCount == 0 && len(p.Accounts) == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{
				"message": "No accounts in pool",
				"type":    "auth_error",
			},
		})
		return
	}

	resp, acc, err := callClineAPI(r.Context(), openAIReq, req.Stream)
	if effectiveModel, ok := openAIReq["model"].(string); ok && effectiveModel != "" {
		reqLog.Model = effectiveModel
	}
	if err != nil {
		log.Printf("  anthropic api error: %v", err)
		finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
		writeUpstreamError(w, err)
		return
	}
	reqLog.Upstream = upstreamCline
	defer resp.Body.Close()
	if acc != nil {
		reqLog.AccountID = acc.AccountID
		reqLog.AccountEmail = acc.Email
	}

	if req.Stream {
		handleAnthropicStream(w, resp, acc, &reqLog)
	} else {
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, "decode response: "+err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		out := raw
		if data, ok := raw["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				out = d
			}
		}
		out = normalizeOpenAIResponse(out)
		usage := parseTokenUsage(out["usage"])
		recordTokenUsage(acc, reqLog.Model, usage)
		finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
		// openAIToAnthropic 已正确保留文本块并映射 stop_reason（P2-10）
		anthropicResp := openAIToAnthropic(out)

		writeJSON(w, http.StatusOK, anthropicResp)
	}
}

func handleAnthropicStream(w http.ResponseWriter, upstream *http.Response, acc *Account, reqLog *RequestLog) {
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	var writeErr error
	emit := func(event string, data any) {
		if writeErr != nil {
			return
		}
		d, _ := json.Marshal(data)
		if _, err := w.Write([]byte(fmt.Sprintf("event: %s\n", event))); err != nil {
			writeErr = err
			return
		}
		if _, err := w.Write([]byte(fmt.Sprintf("data: %s\n\n", string(d)))); err != nil {
			writeErr = err
			return
		}
		flusher.Flush()
	}

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	streamErr := false
	streamErrMsg := ""
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          msgID,
			"type":        "message",
			"role":        "assistant",
			"content":     []any{},
			"model":       "",
			"stop_reason": nil,
		},
	})

	// content_block 统一 index 计数：文本与工具块共用一个递增计数器，
	// 保证同一条流内 index 唯一（Anthropic 契约：块按 index 顺序 open→close）。
	nextBlockIdx := 0
	textIdx := -1
	hasText := false
	textOpen := false
	pendingTools := map[int]*toolAccumulator{}
	var openTool *toolAccumulator

	closeTextBlock := func() {
		if textOpen {
			emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": textIdx,
			})
			textOpen = false
		}
	}
	closeOpenTool := func() {
		if openTool != nil && openTool.open {
			emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": openTool.index,
			})
			openTool.open = false
			openTool.closed = true
		}
	}
	startToolBlock := func(acc *toolAccumulator) {
		if acc.started {
			return
		}
		// 块必须顺序开闭：先收掉还开着的文本块与前一个工具块
		closeTextBlock()
		closeOpenTool()
		acc.started = true
		acc.open = true
		acc.index = nextBlockIdx
		nextBlockIdx++
		// Anthropic 规范：content_block_start 的 input 恒为 {}，参数经 input_json_delta 传递
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": acc.index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    acc.id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		openTool = acc
	}
	emitToolArgsDelta := func(acc *toolAccumulator) {
		if len(acc.args) > acc.sentArgs {
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": acc.index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": acc.args[acc.sentArgs:],
				},
			})
			acc.sentArgs = len(acc.args)
		}
	}

	reader := bufio.NewReader(upstream.Body)
	var latestUsage tokenUsage
	var firstOutputAt time.Time

	for {
		if writeErr != nil {
			break // 客户端已断开/停滞，停止读取上游（P1-4）
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && line != "" && writeErr == nil {
				// 最后一个无换行的 SSE 行也要处理（与 OpenAI 透传路径一致）
				if strings.HasPrefix(line, "data:") {
					if payload := strings.TrimSpace(line[5:]); payload != "" && payload != "[DONE]" {
						var obj map[string]any
						if json.Unmarshal([]byte(payload), &obj) == nil {
							if usage := parseTokenUsage(obj["usage"]); usage.Valid {
								latestUsage = mergeTokenUsage(latestUsage, usage)
							}
						}
					}
				}
			}
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}
		if usage := parseTokenUsage(obj["usage"]); usage.Valid {
			latestUsage = mergeTokenUsage(latestUsage, usage)
		}
		if firstOutputAt.IsZero() && hasFirstOutput(obj) {
			firstOutputAt = time.Now()
		}

		// Detect upstream SSE error
		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			streamErr = true
			streamErrMsg = truncate(string(errBody), 200)
			break
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		// Text content delta
		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				textIdx = nextBlockIdx
				nextBlockIdx++
				textOpen = true
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": textIdx,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": textIdx,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		// Tool calls - accumulate；id/name 齐备即开块，此后每个参数片段实时以
		// input_json_delta 透传（修复：旧实现首个非空片段即整块发射且丢弃后续参数）。
		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{}
					pendingTools[idx] = acc
				}
				if acc.closed {
					log.Printf("  anthropic stream: dropping late fragment for closed tool block (upstream index %d)", idx)
					continue
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						acc.args += args
					}
				}
				if !acc.started {
					if acc.id == "" || acc.name == "" {
						continue // 等后续片段补齐 id/name
					}
					startToolBlock(acc)
				}
				emitToolArgsDelta(acc)
			}
		}

		// Finish reason
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}

	// 上游错误：客户端已收到 error 事件，不再补发工具块/message_delta/message_stop，
	// 请求日志记 failed（P2-12：此前错误后仍补全生命周期事件且日志记成功）
	if streamErr {
		recordTokenUsage(acc, reqLog.Model, latestUsage)
		finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, false, streamErrMsg)
		return
	}

	// 流结束：关闭未闭合块；从未 started 的工具块按上游 index 确定性顺序补发完整三段
	closeTextBlock()
	closeOpenTool()
	toolOrder := make([]int, 0, len(pendingTools))
	for idx := range pendingTools {
		toolOrder = append(toolOrder, idx)
	}
	sort.Ints(toolOrder)
	for _, idx := range toolOrder {
		acc := pendingTools[idx]
		if acc.started {
			continue
		}
		if acc.id == "" && acc.name == "" && acc.args == "" {
			continue
		}
		acc.started = true
		acc.index = nextBlockIdx
		nextBlockIdx++
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": acc.index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    acc.id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		if len(acc.args) > 0 {
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": acc.index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": acc.args,
				},
			})
		}
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": acc.index,
		})
	}

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": latestUsage.Completion,
		},
	})
	recordTokenUsage(acc, reqLog.Model, latestUsage)
	finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, true, "")

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMessage(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					nc["delta"] = nd
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	return out
}

func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return // port is free
	}
	conn.Close()

	// Try to kill the process using the port
	cmd := execCommand("powershell", "-Command",
		fmt.Sprintf(`$p=Get-NetTCPConnection -LocalPort %d -ErrorAction SilentlyContinue; if($p){Stop-Process -Id $p.OwningProcess -Force}`, port))
	_ = cmd.Run()
	time.Sleep(500 * time.Millisecond)
}
