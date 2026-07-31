package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestHTTPTransportUsesHTTPSProxyFromEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8080")
	t.Setenv("NO_PROXY", "")

	req, err := http.NewRequest(http.MethodPost, "https://api.workos.com/user_management/authorize/device", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	proxyURL, err := httpTransport.Proxy(req)
	if err != nil {
		t.Fatalf("resolve proxy: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected HTTPS_PROXY to be selected")
	}

	want, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("parse expected proxy URL: %v", err)
	}
	if proxyURL.String() != want.String() {
		t.Fatalf("proxy URL = %q, want %q", proxyURL, want)
	}
}
