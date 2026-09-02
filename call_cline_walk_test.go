package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// setupClineWalkTest 替换全局池/配置/传输层并在测试结束后还原（free_model_test.go 同款手法）。
// 策略固定为 fill：总是选中池序首个可用账号，使断言确定。
func setupClineWalkTest(t *testing.T, rt freeModelRoundTripper, accounts ...*Account) {
	t.Helper()
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	oldAuthTransport := authClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
		authClient.Transport = oldAuthTransport
	})
	pool = &AccountPool{Accounts: accounts, Keys: []string{}, Models: []Model{}}
	cfg := defaultProxyConfig()
	cfg.Strategy = "fill"
	setProxyConfig(cfg)
	httpClient.Transport = rt
	authClient.Transport = rt
	// 5xx 重试等待调短，避免测试真实睡眠
	oldDelay := cline5xxRetryDelay
	cline5xxRetryDelay = time.Millisecond
	t.Cleanup(func() { cline5xxRetryDelay = oldDelay })
}

func walkTestAccount(id, token string) *Account {
	return &Account{
		AccountID:    id,
		Email:        id + "@example.com",
		RefreshToken: id + "-refresh",
		AccessToken:  token,
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "active",
	}
}

func fakeClineResp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

// TestCallClineAPIWalksAccountsOnUpstream5xx：普通模型上游 5xx 时先同账号重试一次、
// 仍 5xx 则换账号重试，账号 A 持续 500、账号 B 返回 200 → 最终 200 且用到 B。
func TestCallClineAPIWalksAccountsOnUpstream5xx(t *testing.T) {
	first := walkTestAccount("walk-a", "token-a")
	second := walkTestAccount("walk-b", "token-b")
	counts := map[string]int{}
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/v1/chat/completions" {
			return nil, fmt.Errorf("unexpected request path %s", req.URL.Path)
		}
		auth := req.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "token-a"):
			counts["a"]++
			return fakeClineResp(req, http.StatusInternalServerError, `{"error":"upstream boom"}`), nil
		case strings.Contains(auth, "token-b"):
			counts["b"]++
			return fakeClineResp(req, http.StatusOK, `{"id":"ok","choices":[]}`), nil
		}
		return nil, fmt.Errorf("unexpected authorization %q", auth)
	})
	setupClineWalkTest(t, rt, first, second)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if acc != second {
		t.Fatalf("served by account %v, want second account", acc)
	}
	// A 被同账号重试 2 次后换到 B（B 一次成功）
	if counts["a"] != 2 || counts["b"] != 1 {
		t.Fatalf("upstream hits = %v, want a:2 b:1", counts)
	}
}

// TestCallClineAPIRetriesSameAccountOn5xx：上游 5xx 为按请求随机的暂态故障（实测），
// 同账号 ~1s 后重试一次即可恢复——单账号池第一次 500、第二次 200 → 最终 200，
// 上游共命中 2 次。
func TestCallClineAPIRetriesSameAccountOn5xx(t *testing.T) {
	only := walkTestAccount("walk-only", "token-only")
	calls := 0
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fakeClineResp(req, http.StatusInternalServerError, `{"error":"flaky gateway"}`), nil
		}
		return fakeClineResp(req, http.StatusOK, `{"id":"ok","choices":[]}`), nil
	})
	setupClineWalkTest(t, rt, only)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one retry on same account)", calls)
	}
}

// TestCallClineAPI5xxRetryBounded：同账号 5xx 重试至多一次，持续 500 的单账号池
// 恰好命中上游 2 次后透传 clineAPIError{500}，不无限重试。
func TestCallClineAPI5xxRetryBounded(t *testing.T) {
	only := walkTestAccount("walk-only", "token-only")
	calls := 0
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return fakeClineResp(req, http.StatusInternalServerError, `{"error":"dead gateway"}`), nil
	})
	setupClineWalkTest(t, rt, only)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("want error when upstream always 500, got nil")
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || apiErr.statusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want clineAPIError{500}", err)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (bounded retry)", calls)
	}
}

// TestCallClineAPIAllAccountsUpstream5xxPassthrough：所有账号都 5xx 时透传最后的
// clineAPIError（状态码语义不变），且每账号至多试一次、不无限循环。
func TestCallClineAPIAllAccountsUpstream5xxPassthrough(t *testing.T) {
	first := walkTestAccount("walk-a", "token-a")
	second := walkTestAccount("walk-b", "token-b")
	calls := 0
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return fakeClineResp(req, http.StatusInternalServerError, `{"error":"upstream boom"}`), nil
	})
	setupClineWalkTest(t, rt, first, second)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err == nil {
		resp.Body.Close()
		t.Fatal("want error when every account 5xx, got nil")
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || apiErr.statusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want clineAPIError{500}", err)
	}
	if upstreamErrorHTTPStatus(err) != http.StatusInternalServerError {
		t.Fatalf("client status = %d, want 500", upstreamErrorHTTPStatus(err))
	}
	// 每账号同账号重试一次后换下一个：2 账号 × 2 次 = 4
	if calls != 4 {
		t.Fatalf("upstream calls = %d, want 4 (one 5xx retry per account)", calls)
	}
}

// TestCallClineAPIEmptyPoolUnavailableError：池中无账号时返回
// clineAccountUnavailableError（客户端文案 "no account available for this request"），
// 不再伪装成与真实上游故障无法区分的通用 500 文案。
func TestCallClineAPIEmptyPoolUnavailableError(t *testing.T) {
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		t.Error("upstream must not be called with an empty pool")
		return fakeClineResp(req, http.StatusOK, `{}`), nil
	})
	setupClineWalkTest(t, rt) // 空池

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("want error with empty pool, got nil")
	}
	var unavail *clineAccountUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("err = %v, want clineAccountUnavailableError", err)
	}
	if got := upstreamClientMessage(err); got != "no account available for this request" {
		t.Fatalf("client message = %q, want %q", got, "no account available for this request")
	}
}

// TestCallClineAPINoRetryOnUpstream4xx：4xx 是账号级/请求级问题，换账号无意义
// （与 free 链语义一致），不得触发换账号重试——仅请求上游一次。
func TestCallClineAPINoRetryOnUpstream4xx(t *testing.T) {
	first := walkTestAccount("walk-a", "token-a")
	second := walkTestAccount("walk-b", "token-b")
	calls := 0
	rt := freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return fakeClineResp(req, http.StatusForbidden, `{"error":"forbidden"}`), nil
	})
	setupClineWalkTest(t, rt, first, second)

	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, _, err := callClineAPI(context.Background(), params, false)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("want error on upstream 403, got nil")
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || apiErr.statusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want clineAPIError{403}", err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (4xx must not retry)", calls)
	}
}
