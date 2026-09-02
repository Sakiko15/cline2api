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

// TestCallClineAPIWalksAccountsOnUpstream5xx：普通模型上游 5xx 时依序换账号重试，
// 账号 A 返回 500、账号 B 返回 200 → 最终 200 且用到 B（修复前会直接透传 A 的 500，
// 之后每次请求都命中同一账号的同一个 500）。
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
	if counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("upstream hits = %v, want a:1 b:1", counts)
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
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per account)", calls)
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
