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
		// 同 ID 首见优先：用户自定义 → 远程 → zen（P2-18，自定义条目覆盖同步来源）
		return dedupeModelsByID(custom, remote, zen)
	}

	builtin := make([]Model, 0, len(builtinModels)+len(zenSeedModels))
	builtin = append(builtin, builtinModels...)
	builtin = append(builtin, builtinZenModels()...)

	return dedupeModelsByID(custom, builtin)
}

// dedupeModelsByID 按模型 ID 去重拼接多个模型组，首见优先（P2-18）。
func dedupeModelsByID(groups ...[]Model) []Model {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	result := make([]Model, 0, total)
	seen := make(map[string]bool, total)
	for _, g := range groups {
		for _, m := range g {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			result = append(result, m)
		}
	}
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

// restartMu 串行化 restartListener 全流程（P5-4）：并发两次重启会互相覆盖
// currentServer/listenHost/listenPort；Serve 长驻不持锁，本锁仅在流程间互斥。
var restartMu sync.Mutex

// restartListener 用新地址重启 HTTP 监听。
// 注意：必须在 goroutine 中调用——Shutdown 会等待当前 HTTP 请求完成，
// 若在 admin handler 内同步调用会死锁。
func restartListener(host string, port int) error {
	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	restartMu.Lock()
	defer restartMu.Unlock()

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
	// （P1-9：旧实现先 Shutdown 再 ListenAndServe，绑定失败会导致零监听、全代理下线）；
	// P5-4：仅当失败地址就是旧监听自身（同址自占用；旧 Addr 为空是测试构造的
	// 裸 Server）才停旧重试——换址冲突时停旧再失败会制造零监听下线。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		sameAddr := old != nil && (old.Addr == addr || old.Addr == "")
		if !sameAddr {
			return fmt.Errorf("bind %s: %w", addr, err)
		}
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
	listenHost = host
	listenPort = port
	serverMu.Unlock()

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
	// P4-6：OpenAI 规范新增字段直通（上游按需忽略不认识的字段）。
	// moderation / prompt_cache_options 不透传：前者是独立端点语义，后者
	// 属于基础设施路由提示，透传无收益。
	"service_tier", "store", "verbosity", "modalities", "audio", "prediction",
	"web_search_options", "safety_identifier", "prompt_cache_key",
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
					log.Printf("  Pre-warm failed for %s: %v", sanitizeLog(truncateEmail(a.Email), 64), sanitizeLog(err.Error(), 256))
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

	// 端口被占用时拒绝启动并指明占用进程（P2-17，替代旧的 PowerShell 强杀）
	if err := ensurePortFree(host, port); err != nil {
		return err
	}

	mux := http.NewServeMux()

	// 健康端点只回 status（P3-11）：version/账号数此前无鉴权可指纹；
	// 已知消费方（docker healthcheck、desktop selfcheck）只看状态码
	mux.HandleFunc("/v1/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	// Admin API (frontend + REST)
	// 管理密码一次性引导：既有哈希 > CLINE_ADMIN_PASSWORD 环境变量 > 随机
	// 生成并打印到日志（配合管理面 fail-closed，公网实例无需本机登录即可
	// 获得初始密码；已设密码的实例完全不受影响）
	bootstrapAdminPassword()

	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			// P5-9：锁内快照 Keys（与管理端并发增删键时无锁遍历是数据竞争）；
			// 恒时比较逐键全量、不提前结束的语义不变（P2-3）
			keys := snapshotPoolKeys()
			if len(keys) == 0 {
				next(w, r)
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			if !apiKeyValid(key, keys) {
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
		// 只在池内完全无账号时拒绝（P3-3：启动快照 activeCount 会陈旧）
		if len(loadPool().Accounts) == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"message": "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.",
					"type":    "auth_error",
				},
			})
			return
		}

		body, ok := readChatBody(w, r)
		if !ok {
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
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, sanitizeLog(model, 128))

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

		if len(loadPool().Accounts) == 0 {
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
		// 纵深防御：管理面已拒绝受保护头的覆盖，旧配置文件中可能仍有残留（P2-16）
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Content-Type", "Host", "Content-Length":
			log.Printf("  ignoring protected header override from config: %s", k)
			continue
		}
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

// upstreamClientMessage 计算 /v1 客户端可见的错误描述（P3-10）：只暴露状态码
// 与本地固定文案，上游原文（可含账号 email、内部路径、上游响应体）仅保留在
// 管理端请求日志。状态码映射仍由 upstreamErrorHTTPStatus 决定，互不影响。
func upstreamClientMessage(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "request canceled"
	}
	var zerr *zenAPIError
	if errors.As(err, &zerr) {
		return fmt.Sprintf("opencode upstream returned HTTP %d", zerr.statusCode)
	}
	if ferr, ok := err.(*freeModelUnavailableError); ok {
		return ferr.message // 本地固定文案，非上游回显
	}
	var uerr *clineAccountUnavailableError
	if errors.As(err, &uerr) { // 原文含账号 email，不外发
		return "no account available for this request"
	}
	var aerr *clineAPIError
	if errors.As(err, &aerr) {
		return fmt.Sprintf("upstream returned HTTP %d", aerr.statusCode)
	}
	return "upstream request failed"
}

// writeUpstreamError 统一 /v1 错误响应：按上游状态映射状态码，429 回填 Retry-After。
// 客户端 message 走 upstreamClientMessage 最小化回显（P3-10）；
// Retry-After 仍从错误原文解析冷却文案（P1-8 语义保持）。
func writeUpstreamError(w http.ResponseWriter, err error) {
	status := upstreamErrorHTTPStatus(err)
	if status == http.StatusTooManyRequests {
		if d := retryAfterFor(err); d > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		}
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": upstreamClientMessage(err), "type": "api_error"},
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
	var lastErr error
	for _, model := range freeModelChain {
		params["model"] = model
		// tried 按链内模型独立：429 冷却账号本就被资格集排除；非 429 的 4xx
		// 不产生冷却，靠 tried 保证每账号每模型至多试一次（P5-2）
		tried := make(map[*Account]struct{})
		for {
			if err := ctx.Err(); err != nil {
				// 客户端取消：立即终止，不做徒劳的账号遍历（P1-4 语义保持：
				// 取消映射为账号不可用错误，不冷却账号）
				return nil, nil, &clineAccountUnavailableError{err: fmt.Errorf("upstream request canceled: %w", err)}
			}
			acc := pickAccountForModelStrictExcept(model, tried)
			if acc == nil {
				break
			}
			tried[acc] = struct{}{}

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
			// 链内账号耗尽后由外层循环尝试下一链模型（P1-11）；
			// 非 429 的 4xx 记为链尾候选错误（P5-2：修复原地无限重选死循环）
			if apiErr.statusCode != 429 {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return nil, nil, lastErr
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
		sanitizeLog(truncateEmail(acc.Email), 64), stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

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
		bodyBytes := readAllLimited(resp.Body, 64<<10) // 错误体限额读取（P2-8）
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
	safeGo("cooldown-recovery", func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		tick := 0
		for range ticker.C {
			tick++
			guardTick("cooldown-recovery", func() {
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
					log.Printf("cooldown recovery: testing %s", sanitizeLog(truncateEmail(acc.Email), 64))
					result := testAccount(acc)
					if result.OK {
						log.Printf("cooldown recovery: %s reactivated", sanitizeLog(truncateEmail(acc.Email), 64))
					} else {
						log.Printf("cooldown recovery: %s still unavailable: %s", sanitizeLog(truncateEmail(acc.Email), 64), sanitizeLog(result.Error, 256))
					}
				}
				for _, acc := range toReactivate {
					log.Printf("expired account probe: testing %s", sanitizeLog(truncateEmail(acc.Email), 64))
					result := testAccount(acc)
					if result.OK {
						log.Printf("expired account probe: %s reactivated", sanitizeLog(truncateEmail(acc.Email), 64))
					} else {
						log.Printf("expired account probe: %s still unavailable: %s", sanitizeLog(truncateEmail(acc.Email), 64), sanitizeLog(result.Error, 256))
					}
				}
			})
		}
	})
}

// testAccountTimeout 探活请求上界（P5-3）：httpClient 无全局 Timeout，
// 上游挂起曾使恢复循环串行卡死（30s ticker 停摆、恢复永久失效）。
// 健康上游对 trivial 探活请求秒级返回；超时走取消路径不冷却账号。
var testAccountTimeout = 30 * time.Second

// testAccount sends a minimal "hi" request through a specific account to verify
// it can complete an upstream call. It does not update aggregate token counters
// or request logs; it is a diagnostic-only probe.
func testAccount(acc *Account) accountTestResult {
	result := accountTestResult{AccountID: acc.AccountID, Email: acc.Email}
	started := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), testAccountTimeout)
	defer cancel()

	params := map[string]any{
		"model":      getDefaultModel(),
		"max_tokens": 16,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	resp, _, err := callClineAPIWithAccount(ctx, acc, params, false)
	if err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = truncate(err.Error(), 200)
		return result
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 探活响应限额读取（P2-8）
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
	Prompt        int64
	Completion    int64
	Total         int64
	Cached        int64
	CacheRead     int64
	CacheCreation int64
	Valid         bool
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
	// 缓存读/写分开记（Anthropic usage 需要两个独立字段）；与上面 Cached 链各自独立，
	// 读序沿用 nested cached_tokens 优先、显式键兜底。
	cacheRead := int64(0)
	if nested := readNested("prompt_tokens_details", "cached_tokens"); nested > 0 {
		cacheRead = nested
	} else if nested := readNested("input_tokens_details", "cached_tokens"); nested > 0 {
		cacheRead = nested
	} else {
		cacheRead = read("cache_read_input_tokens")
	}
	cacheCreation := read("cache_creation_input_tokens")
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
	return tokenUsage{Prompt: prompt, Completion: completion, Total: total, Cached: cached, CacheRead: cacheRead, CacheCreation: cacheCreation, Valid: hasUsage}
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
	if next.CacheRead != 0 {
		current.CacheRead = next.CacheRead
	}
	if next.CacheCreation != 0 {
		current.CacheCreation = next.CacheCreation
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
	markPoolDirtyLocked()
	poolMu.Unlock()
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
		markPoolDirtyLocked()
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
	markPoolDirtyLocked()
	poolMu.Unlock()
	log.Printf("model cooldown: account=%s model=%s until=%s", sanitizeLog(truncateEmail(acc.Email), 64), sanitizeLog(model, 128), until.Format("15:04:05"))
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
				unwrapped := false
				target := obj
				if data, ok := obj["data"]; ok {
					if d, ok := data.(map[string]any); ok {
						if _, hasChoices := d["choices"]; hasChoices {
							target = d
							unwrapped = true
						}
						if _, hasID := d["id"]; hasID {
							target = d
							unwrapped = true
						}
					}
				}
				normalized := normalizeOpenAIResponse(target)
				if usage := parseTokenUsage(normalized["usage"]); usage.Valid {
					latestUsage = mergeTokenUsage(latestUsage, usage)
				}
				if firstOutputAt.IsZero() && hasFirstOutput(normalized) {
					firstOutputAt = time.Now()
				}
				// P5-11：干净 chunk（未解包信封、零修正命中）原样透传上游
				// 字节——JSON 值恒等（差异仅键序/转义/数字书写），省去逐 chunk
				// 的 Go 重序列化；命中修正或解包仍走重序列化，语义修正无一丢失
				if !unwrapped && !needsNormalize(target) {
					if !writeChunk([]byte("data: " + payload + "\n\n")) {
						break
					}
					continue
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
	index    int // Anthropic content_block index（content_block_start 时分配，不复用上游序号）
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

// overrideFilePath override.md 的解析路径（包级 var 便于测试；与其他数据文件一致
// 走 resolveDataPath 搜索链，docker 挂载 /app 下仍被 exe/cwd 候选命中）。
var overrideFilePath = resolveDataPath("override.md")

// overrideMaxBytes override.md 读取上限：超限截断并一次性告警（P3-14）。
const overrideMaxBytes = 256 << 10

// override.md 的 mtime+size 缓存（P3-14）：未变化只做 os.Stat，不再每请求读盘。
var (
	overrideCacheMu   sync.Mutex
	overrideCacheMod  time.Time
	overrideCacheSize int64
	overrideCacheBody string
	overrideCacheOK   bool // 缓存有效（含「文件不存在」与「文件为空」两态，避免反复读）
	overrideMissWarn  bool // 首次「不存在」已提示，其后静默
	overrideCapWarn   bool // 首次「超限截断」已提示
)

// loadOverrideContent 读取 override.md（mtime+size 缓存；相对路径改为与其他
// 数据文件一致的 resolveDataPath 解析；256KiB 上限；不存在的提示只打一次）。
func loadOverrideContent() string {
	overrideCacheMu.Lock()
	defer overrideCacheMu.Unlock()

	fi, err := os.Stat(overrideFilePath)
	if err != nil {
		if overrideCacheOK {
			overrideCacheOK = false // 文件被删除，缓存失效
		}
		if !overrideMissWarn {
			overrideMissWarn = true
			log.Printf("  override.md not found: %v (suppressing further not-found logs)", err)
		}
		return ""
	}
	if overrideCacheOK && fi.ModTime().Equal(overrideCacheMod) && fi.Size() == overrideCacheSize {
		return overrideCacheBody
	}
	data, err := os.ReadFile(overrideFilePath)
	if err != nil {
		// stat 与 read 之间的竞态（文件被删/权限变化）：视同不存在
		overrideCacheOK = false
		return ""
	}
	if len(data) > overrideMaxBytes {
		data = data[:overrideMaxBytes]
		if !overrideCapWarn {
			overrideCapWarn = true
			log.Printf("  override.md exceeds %d bytes, truncated", overrideMaxBytes)
		}
	}
	content := strings.TrimSpace(string(data))
	overrideCacheMod, overrideCacheSize, overrideCacheBody, overrideCacheOK = fi.ModTime(), fi.Size(), content, true
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

// imageUnsupportedOnce P4-4：不支持的 image source 形态只告警一次，避免刷日志
var imageUnsupportedOnce sync.Once

// reasoningDroppedOnce P4-5：工具块打开期间的推理分片丢弃只告警一次
var reasoningDroppedOnce sync.Once

// anthropicPingInterval 流式空闲 ping 间隔（P4-7）；var 便于测试收缩
var anthropicPingInterval = 15 * time.Second

// imageBlockToOpenAI 把 Anthropic image 块转成 OpenAI image_url part（P4-4）。
// base64 → data URL；url source → 原样透传；其余/缺字段形态返回 false 由调用方告警。
func imageBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	switch source["type"] {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType == "" || data == "" {
			return nil, false
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data},
		}, true
	case "url":
		if u, ok := source["url"].(string); ok && u != "" {
			return map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": u},
			}, true
		}
	}
	return nil, false
}

// userContentMessage 组装可能含图片的 user 轮内容（P4-4）：text part 在前、
// 图片 parts 在后；无图片回落纯文本字符串；全空返回 nil（调用方不得追加空消息）。
func userContentMessage(textParts []string, imageParts []any) map[string]any {
	if len(imageParts) == 0 {
		if len(textParts) == 0 {
			return nil
		}
		return map[string]any{"role": "user", "content": strings.Join(textParts, "\n")}
	}
	parts := []any{}
	if len(textParts) > 0 {
		parts = append(parts, map[string]any{"type": "text", "text": strings.Join(textParts, "\n")})
	}
	parts = append(parts, imageParts...)
	return map[string]any{"role": "user", "content": parts}
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
			var imageParts []any  // P4-4：Anthropic image 块转 OpenAI image_url（仅 user 轮）

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// P4-4：仅 user 轮转换（assistant 图片在 OpenAI 请求里没有
						// 合法承载位，继续跳过）；不支持的 source 形态告警一次后跳过
						if part, ok := imageBlockToOpenAI(b); ok && m.Role == "user" {
							imageParts = append(imageParts, part)
						} else if !ok {
							imageUnsupportedOnce.Do(func() {
								log.Printf("  anthropic: unsupported image source type, block skipped (upstream support unverified)")
							})
						}
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
						content := b["content"]
						// is_error 仅在 string content 上加前缀传递（P4-3）：OpenAI tool
						// 消息没有 is_error 字段，丢弃标记会让模型把工具失败当成功；
						// array 形态保持原样，避免 reshaping 破坏结构化结果
						if isErr, _ := b["is_error"].(bool); isErr {
							if s, ok := content.(string); ok {
								content = "[tool_error] " + s
							}
						}
						toolResults = append(toolResults, map[string]any{
							"role":         "tool",
							"content":      content,
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
			} else if m.Role == "user" && (len(toolResults) > 0 || len(imageParts) > 0) {
				// tool_result 与图片可同轮并存（Anthropic 混合轮）；user 轮文本
				// 不再丢弃（P4-2），textParts 为空且无图片时不追加空 user 消息
				msgs = append(msgs, toolResults...)
				if u := userContentMessage(textParts, imageParts); u != nil {
					msgs = append(msgs, u)
				}
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
		out["stop_sequence"] = nil
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}

	text := ""
	reasoning := ""
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
		// P4-5：上游 reasoning_content → thinking 块；不带 signature 字段
		//（透传响应无签名语义，伪造无意义，官方 SDK 建模上 signature 可缺省）
		if r, ok := msg["reasoning_content"].(string); ok {
			reasoning = sanitizeContent(r)
		}
	}

	hasToolCalls := false
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			hasToolCalls = true
		}
	}

	// thinking 块恒在 content 首位（Anthropic 惯例：思考先于回答）
	var contentBlocks []any
	if reasoning != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	// 无工具调用时保持旧行为：恒带文本块（可为空串）
	if !hasToolCalls || text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}

	// Convert tool_calls to Anthropic tool_use blocks
	if hasToolCalls {
		tc, _ := msg["tool_calls"].([]any)
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

	out["content"] = contentBlocks

	stopReason := "end_turn"
	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	case "content_filter":
		stopReason = "refusal"
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
	out["stop_sequence"] = nil

	usage := map[string]any{
		"input_tokens":                0,
		"output_tokens":               0,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
	}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			// 仅在上游真的给出该键时覆盖，避免 usage 存在但缺 prompt_tokens 时输出 null/0 混排
			if v, ok := um["prompt_tokens"]; ok {
				usage["input_tokens"] = v
			}
			if v, ok := um["completion_tokens"]; ok {
				usage["output_tokens"] = v
			}
			if tu := parseTokenUsage(u); tu.Valid {
				if tu.CacheRead > 0 {
					usage["cache_read_input_tokens"] = tu.CacheRead
				}
				if tu.CacheCreation > 0 {
					usage["cache_creation_input_tokens"] = tu.CacheCreation
				}
			}
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := readChatBody(w, r)
	if !ok {
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

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", sanitizeLog(req.Model, 128), req.Stream, len(req.Messages))

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

	p := loadPool()
	if len(p.Accounts) == 0 { // P3-3：实时判池空，不用启动快照
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

	// P4-7：emit 收进互斥——主循环与空闲 ping goroutine 并发写同一 ResponseWriter
	var writeErrMu sync.Mutex
	var writeErr error
	emit := func(event string, data any) {
		writeErrMu.Lock()
		defer writeErrMu.Unlock()
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
	writeFailed := func() bool {
		writeErrMu.Lock()
		defer writeErrMu.Unlock()
		return writeErr != nil
	}

	// P4-7：空闲 ping。cline 上游长思考期可能 >60s 零字节，中间 LB 的默认空闲
	// 超时会掐断连接；官方流式实现同样周期性发 ping 保活。goroutine 启动前先
	// 注册 defer，覆盖所有提前 return 路径；handler 返回前等待 goroutine 退出，
	// 杜绝 ResponseWriter 在 handler 返回后被写。
	done := make(chan struct{})
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	defer func() {
		close(done)
		pingWG.Wait()
	}()
	safeGo("anthropic-ping", func() {
		defer pingWG.Done()
		ticker := time.NewTicker(anthropicPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				emit("ping", map[string]any{"type": "ping"})
			}
		}
	})

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	streamErr := false
	streamErrMsg := ""
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"model":   reqLog.Model,
			// Anthropic 规范：message_start 携带完整 message 形状，usage 从 0 起步
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	// content_block 统一 index 计数：文本/思考/工具块共用一个递增计数器，
	// 保证同一条流内 index 唯一（Anthropic 契约：块按 index 顺序 open→close）。
	nextBlockIdx := 0
	textIdx := -1
	hasText := false
	textOpen := false
	thinkIdx := -1
	thinkingOpen := false
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
	closeThinkingBlock := func() {
		if thinkingOpen {
			emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": thinkIdx,
			})
			thinkingOpen = false
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
		// 块必须顺序开闭：先收掉还开着的文本块/思考块与前一个工具块
		closeTextBlock()
		closeThinkingBlock()
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
		if writeFailed() {
			break // 客户端已断开/停滞，停止读取上游（P1-4）
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && line != "" && !writeFailed() {
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
			// P3-10：客户端只收固定文案，上游原文仅入日志与 streamErrMsg
			emit("error", map[string]any{"type": "upstream_error", "message": "upstream returned an error during streaming"})
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

		// Reasoning content → thinking 块（P4-5）。懒开：首个推理分片到达时开新块；
		// 发 thinking_delta、不发 signature_delta（透传无签名语义）。
		// 工具块打开期间到达的推理分片丢弃——关闭工具块会截断 input_json_delta。
		if r, ok := delta["reasoning_content"].(string); ok && r != "" {
			if openTool != nil && openTool.open {
				reasoningDroppedOnce.Do(func() {
					log.Printf("  anthropic stream: dropping reasoning_content while tool block open")
				})
			} else {
				closeTextBlock()
				if !thinkingOpen {
					thinkingOpen = true
					thinkIdx = nextBlockIdx
					nextBlockIdx++
					emit("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": thinkIdx,
						"content_block": map[string]any{
							"type":     "thinking",
							"thinking": "",
						},
					})
				}
				emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": thinkIdx,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": sanitizeContent(r),
					},
				})
			}
		}

		// Text content delta
		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				// 块顺序开闭：首个文本分片前先收掉思考块
				closeThinkingBlock()
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
			case "content_filter":
				stopReason = "refusal"
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
	closeThinkingBlock()
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

	deltaUsage := map[string]any{
		"input_tokens":  latestUsage.Prompt,
		"output_tokens": latestUsage.Completion,
	}
	if latestUsage.CacheCreation > 0 {
		deltaUsage["cache_creation_input_tokens"] = latestUsage.CacheCreation
	}
	if latestUsage.CacheRead > 0 {
		deltaUsage["cache_read_input_tokens"] = latestUsage.CacheRead
	}
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": deltaUsage,
	})
	recordTokenUsage(acc, reqLog.Model, latestUsage)
	finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, true, "")

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}

// hasStripKeys 报告 m 是否含会被剥离的 provider/proxy_metadata 键。
func hasStripKeys(m map[string]any) bool {
	_, a := m["provider_metadata"]
	_, b := m["proxy_metadata"]
	return a || b
}

// needsMessageNormalize：message/delta 层的修正条件——剥离键或
// tool_calls 非空且 content 为 nil 的注入。sanitizeContent 恒等不计。
func needsMessageNormalize(m map[string]any) bool {
	if hasStripKeys(m) {
		return true
	}
	if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 && m["content"] == nil {
		return true
	}
	return false
}

// needsNormalize O(浅层) 判定 obj 是否命中 normalizeOpenAIResponse 的任何
// 修正（P5-11）：顶层/choice/message/delta 四层剥离键，或 message/delta 的
// tool_calls 内容注入。全不命中时 normalizeOpenAIResponse 的输出与输入
// JSON 值恒等（sanitizeContent 恒等），可零分配返回原 map。
func needsNormalize(obj map[string]any) bool {
	if hasStripKeys(obj) {
		return true
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, ch := range choices {
		c, ok := ch.(map[string]any)
		if !ok {
			continue // 非对象 choice 原样追加，无修正
		}
		if hasStripKeys(c) {
			return true
		}
		if msg, ok := c["message"].(map[string]any); ok && needsMessageNormalize(msg) {
			return true
		}
		if delta, ok := c["delta"].(map[string]any); ok && needsMessageNormalize(delta) {
			return true
		}
	}
	return false
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	// P5-11 零分配快路径：无任何修正命中时返回原 map（输出 JSON 值恒等）
	if !needsNormalize(obj) {
		return obj
	}
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

// describePortOwner 尽力找出占用本机端口的监听进程名（Windows，3s 超时），失败返回空串。
func describePortOwner(port int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := execCommandContext(ctx, "powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`(Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty OwningProcess | ForEach-Object { (Get-Process -Id $_ -ErrorAction SilentlyContinue).ProcessName })`, port)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ensurePortFree 探测监听地址是否可绑定；被占用时返回含占用进程名的明确错误。
// 不再强杀占用进程（P2-17）：PowerShell 强杀可能误伤无关服务，改为拒绝启动。
func ensurePortFree(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port)) // host 为空 = 所有网卡，与实际 Listen 一致
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return nil
	}
	if owner := describePortOwner(port); owner != "" {
		return fmt.Errorf("port %d is already in use by process %q (%s); stop that process or start with a different -port", port, owner, err)
	}
	return fmt.Errorf("port %d is already in use (%s); stop the occupying process or start with a different -port", port, err)
}
