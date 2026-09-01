package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var execCommand = exec.Command

// 请求体限额（var 便于测试收缩）：聊天类端点 32MB，管理面 1MB（P2-8）。
var (
	maxChatBodyBytes  int64 = 32 << 20
	maxAdminBodyBytes int64 = 1 << 20
)

// readBodyLimited 限额读取请求体，超限返回 *http.MaxBytesError（isBodyTooLarge 判定）。
func readBodyLimited(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	return io.ReadAll(r.Body)
}

func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// readChatBody 限额读取 /v1 聊天类端点请求体（默认 32MB）；失败或超限时已写好响应（P2-8）。
func readChatBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := readBodyLimited(w, r, maxChatBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		if isBodyTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return nil, false
	}
	return body, true
}

var httpTransport = &http.Transport{
	Proxy:                http.ProxyFromEnvironment,
	MaxIdleConns:         100,
	MaxIdleConnsPerHost:  10,
	IdleConnTimeout:      90 * time.Second,
	TLSHandshakeTimeout:  10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	DisableCompression:   false,
}

// httpClient 用于可能长时的流式上游请求：不设 Client.Timeout（会掐断长流），
// 挂起防护依赖请求携带的 context 与 TLS/Continue 超时（P1-3/P1-4）。
var httpClient = &http.Client{
	Transport: httpTransport,
}

// authClient 专用于 token 刷新/注册/设备码轮询等短时鉴权请求（P1-3）。
var authClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: httpTransport,
}

func httpPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return authClient.Do(req)
}

func httpPostJSON(rawURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return authClient.Do(req)
}

func readBody(resp *http.Response) string {
	// 上游错误体只需诊断用途，限额读取防异常大的错误页（P2-8）
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
