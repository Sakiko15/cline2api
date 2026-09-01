package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var execCommand = exec.Command

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
	data, err := io.ReadAll(resp.Body)
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
