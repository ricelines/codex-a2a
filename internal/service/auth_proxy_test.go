package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestChatGPTAccountIDFromAccessToken(t *testing.T) {
	token := makeChatGPTAccessToken(t, "org-123")

	got, err := chatGPTAccountIDFromAccessToken(token)
	if err != nil {
		t.Fatalf("chatGPTAccountIDFromAccessToken() error = %v", err)
	}
	if got != "org-123" {
		t.Fatalf("chatGPTAccountIDFromAccessToken() = %q, want %q", got, "org-123")
	}
}

func TestAuthProxyStripsCallerHeadersAndInjectsTrustedAuth(t *testing.T) {
	var gotAuth string
	var gotAccountID string
	var gotBody string
	handler, err := NewAuthProxyHandler(
		"http://api.invalid/v1/responses",
		"http://upstream.invalid/backend-api/codex/responses",
		&stubAuthTokenProvider{
			auth: proxyAuth{
				Method:      proxyAuthMethodChatGPT,
				BearerToken: makeChatGPTAccessToken(t, "org-trusted"),
				AccountID:   "org-trusted",
			},
		},
	)
	if err != nil {
		t.Fatalf("NewAuthProxyHandler() error = %v", err)
	}
	proxy := handler.(*authProxyHandler)
	proxy.httpClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream got %s %s, want POST /backend-api/codex/responses", req.Method, req.URL.Path)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll(req.Body) error = %v", err)
		}
		gotAuth = req.Header.Get("Authorization")
		gotAccountID = req.Header.Get("ChatGPT-Account-Id")
		gotBody = string(body)

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event: response.completed\ndata: {}\n\n")),
		}, nil
	})}

	req, err := http.NewRequest(http.MethodPost, "http://proxy.local/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer attacker")
	req.Header.Set("ChatGPT-Account-Id", "org-attacker")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if gotAuth != "Bearer "+makeChatGPTAccessToken(t, "org-trusted") {
		t.Fatalf("Authorization header = %q, want trusted bearer token", gotAuth)
	}
	if gotAccountID != "org-trusted" {
		t.Fatalf("ChatGPT-Account-Id header = %q, want trusted account", gotAccountID)
	}
	if gotBody != `{"input":"hello"}` {
		t.Fatalf("forwarded body = %q, want request body", gotBody)
	}
}

func TestAuthProxyForwardsAPIKeyWithoutChatGPTAccountID(t *testing.T) {
	var gotAuth string
	var gotAccountID string
	var gotBody string

	handler, err := NewAuthProxyHandler(
		"http://upstream.invalid/v1/responses",
		"http://chatgpt.invalid/backend-api/codex/responses",
		&stubAuthTokenProvider{
			auth: proxyAuth{
				Method:      proxyAuthMethodAPIKey,
				BearerToken: "sk-trusted",
			},
		},
	)
	if err != nil {
		t.Fatalf("NewAuthProxyHandler() error = %v", err)
	}
	proxy := handler.(*authProxyHandler)
	proxy.httpClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/responses" {
			t.Fatalf("upstream got %s %s, want POST /v1/responses", req.Method, req.URL.Path)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll(req.Body) error = %v", err)
		}
		gotAuth = req.Header.Get("Authorization")
		gotAccountID = req.Header.Get("ChatGPT-Account-Id")
		gotBody = string(body)

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event: response.completed\ndata: {}\n\n")),
		}, nil
	})}

	req, err := http.NewRequest(http.MethodPost, "http://proxy.local/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer attacker")
	req.Header.Set("ChatGPT-Account-Id", "org-attacker")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if gotAuth != "Bearer sk-trusted" {
		t.Fatalf("Authorization header = %q, want trusted bearer token", gotAuth)
	}
	if gotAccountID != "" {
		t.Fatalf("ChatGPT-Account-Id header = %q, want empty for API-key auth", gotAccountID)
	}
	if gotBody != `{"input":"hello"}` {
		t.Fatalf("forwarded body = %q, want request body", gotBody)
	}
}

func TestAuthProxyRetriesUnauthorizedWithRefreshedToken(t *testing.T) {
	var mu sync.Mutex
	var gotAuth []string
	var refreshCalls []bool

	initial := makeChatGPTAccessToken(t, "org-initial")
	refreshed := makeChatGPTAccessToken(t, "org-refreshed")

	handler, err := NewAuthProxyHandler(
		"http://api.invalid/v1/responses",
		"http://upstream.invalid/backend-api/codex/responses",
		&stubAuthTokenProvider{
			authFunc: func(refresh bool) (proxyAuth, error) {
				refreshCalls = append(refreshCalls, refresh)
				if refresh {
					return proxyAuth{
						Method:      proxyAuthMethodChatGPT,
						BearerToken: refreshed,
						AccountID:   "org-refreshed",
					}, nil
				}
				return proxyAuth{
					Method:      proxyAuthMethodChatGPT,
					BearerToken: initial,
					AccountID:   "org-initial",
				}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewAuthProxyHandler() error = %v", err)
	}
	proxy := handler.(*authProxyHandler)
	proxy.httpClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		gotAuth = append(gotAuth, req.Header.Get("Authorization"))
		call := len(gotAuth)
		mu.Unlock()

		if call == 1 {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("expired")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event: response.completed\ndata: {}\n\n")),
		}, nil
	})}

	req, err := http.NewRequest(http.MethodPost, "http://proxy.local/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if len(refreshCalls) != 2 || refreshCalls[0] || !refreshCalls[1] {
		t.Fatalf("refresh call sequence = %#v, want [false true]", refreshCalls)
	}
	if len(gotAuth) != 2 {
		t.Fatalf("upstream call count = %d, want 2", len(gotAuth))
	}
	if gotAuth[0] != "Bearer "+initial {
		t.Fatalf("first Authorization header = %q, want initial token", gotAuth[0])
	}
	if gotAuth[1] != "Bearer "+refreshed {
		t.Fatalf("second Authorization header = %q, want refreshed token", gotAuth[1])
	}
}

func TestAuthProxyDoesNotRetryUnauthorizedWithAPIKey(t *testing.T) {
	var authCalls []bool
	var upstreamCalls int

	handler, err := NewAuthProxyHandler(
		"http://upstream.invalid/v1/responses",
		"http://chatgpt.invalid/backend-api/codex/responses",
		&stubAuthTokenProvider{
			authFunc: func(refresh bool) (proxyAuth, error) {
				authCalls = append(authCalls, refresh)
				return proxyAuth{
					Method:      proxyAuthMethodAPIKey,
					BearerToken: "sk-trusted",
				}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewAuthProxyHandler() error = %v", err)
	}
	proxy := handler.(*authProxyHandler)
	proxy.httpClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls++
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("expired")),
		}, nil
	})}

	req, err := http.NewRequest(http.MethodPost, "http://proxy.local/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if len(authCalls) != 1 || authCalls[0] {
		t.Fatalf("auth call sequence = %#v, want [false]", authCalls)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream call count = %d, want 1", upstreamCalls)
	}
}

func TestCodexAuthTokenProviderUsesHelperProcessForChatGPT(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultCwd = t.TempDir()
	cfg.CodexAppServerBin = os.Args[0]
	cfg.CodexCLI = ""
	cfg.CodexArgs = []string{"-test.run=TestFakeCodexHelperProcess", "--"}
	cfg.ChildEnv = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"FAKE_CODEX_AUTH_TOKEN_INITIAL=" + makeChatGPTAccessToken(t, "org-initial"),
		"FAKE_CODEX_AUTH_TOKEN_REFRESHED=" + makeChatGPTAccessToken(t, "org-refreshed"),
	}

	provider, err := newCodexAuthTokenProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newCodexAuthTokenProvider() error = %v", err)
	}
	defer func() {
		if err := provider.Close(); err != nil {
			t.Fatalf("provider.Close() error = %v", err)
		}
	}()

	initial, err := provider.CurrentAuth(context.Background(), false)
	if err != nil {
		t.Fatalf("CurrentAuth(false) error = %v", err)
	}
	if initial.Method != proxyAuthMethodChatGPT {
		t.Fatalf("initial auth method = %q, want %q", initial.Method, proxyAuthMethodChatGPT)
	}
	if initial.AccountID != "org-initial" {
		t.Fatalf("initial account id = %q, want %q", initial.AccountID, "org-initial")
	}
	if got := mustAccountIDFromToken(t, initial.BearerToken); got != "org-initial" {
		t.Fatalf("initial token account id = %q, want %q", got, "org-initial")
	}

	refreshed, err := provider.CurrentAuth(context.Background(), true)
	if err != nil {
		t.Fatalf("CurrentAuth(true) error = %v", err)
	}
	if refreshed.Method != proxyAuthMethodChatGPT {
		t.Fatalf("refreshed auth method = %q, want %q", refreshed.Method, proxyAuthMethodChatGPT)
	}
	if refreshed.AccountID != "org-refreshed" {
		t.Fatalf("refreshed account id = %q, want %q", refreshed.AccountID, "org-refreshed")
	}
	if got := mustAccountIDFromToken(t, refreshed.BearerToken); got != "org-refreshed" {
		t.Fatalf("refreshed token account id = %q, want %q", got, "org-refreshed")
	}
}

func TestCodexAuthTokenProviderUsesHelperProcessForAPIKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultCwd = t.TempDir()
	cfg.CodexAppServerBin = os.Args[0]
	cfg.CodexCLI = ""
	cfg.CodexArgs = []string{"-test.run=TestFakeCodexHelperProcess", "--"}
	cfg.ChildEnv = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"FAKE_CODEX_AUTH_METHOD=apikey",
		"FAKE_CODEX_AUTH_TOKEN_INITIAL=sk-initial",
		"FAKE_CODEX_AUTH_TOKEN_REFRESHED=sk-refreshed",
	}

	provider, err := newCodexAuthTokenProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newCodexAuthTokenProvider() error = %v", err)
	}
	defer func() {
		if err := provider.Close(); err != nil {
			t.Fatalf("provider.Close() error = %v", err)
		}
	}()

	initial, err := provider.CurrentAuth(context.Background(), false)
	if err != nil {
		t.Fatalf("CurrentAuth(false) error = %v", err)
	}
	if initial.Method != proxyAuthMethodAPIKey {
		t.Fatalf("initial auth method = %q, want %q", initial.Method, proxyAuthMethodAPIKey)
	}
	if initial.BearerToken != "sk-initial" {
		t.Fatalf("initial bearer token = %q, want %q", initial.BearerToken, "sk-initial")
	}
	if initial.AccountID != "" {
		t.Fatalf("initial account id = %q, want empty for API-key auth", initial.AccountID)
	}

	refreshed, err := provider.CurrentAuth(context.Background(), true)
	if err != nil {
		t.Fatalf("CurrentAuth(true) error = %v", err)
	}
	if refreshed.Method != proxyAuthMethodAPIKey {
		t.Fatalf("refreshed auth method = %q, want %q", refreshed.Method, proxyAuthMethodAPIKey)
	}
	if refreshed.BearerToken != "sk-refreshed" {
		t.Fatalf("refreshed bearer token = %q, want %q", refreshed.BearerToken, "sk-refreshed")
	}
	if refreshed.AccountID != "" {
		t.Fatalf("refreshed account id = %q, want empty for API-key auth", refreshed.AccountID)
	}
}

type stubAuthTokenProvider struct {
	auth     proxyAuth
	authFunc func(bool) (proxyAuth, error)
}

func (p *stubAuthTokenProvider) CurrentAuth(_ context.Context, refresh bool) (proxyAuth, error) {
	if p.authFunc != nil {
		return p.authFunc(refresh)
	}
	return p.auth, nil
}

func (p *stubAuthTokenProvider) Close() error {
	return nil
}

func makeChatGPTAccessToken(t *testing.T, accountID string) string {
	t.Helper()

	header := encodeJWTPart(t, map[string]any{"alg": "none", "typ": "JWT"})
	payload := encodeJWTPart(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	return header + "." + payload + ".signature"
}

func encodeJWTPart(t *testing.T, payload map[string]any) string {
	t.Helper()

	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(blob)
}

func mustAccountIDFromToken(t *testing.T, token string) string {
	t.Helper()

	accountID, err := chatGPTAccountIDFromAccessToken(token)
	if err != nil {
		t.Fatalf("chatGPTAccountIDFromAccessToken() error = %v", err)
	}
	return accountID
}

func newLoopbackHTTPServer(t *testing.T, handler http.Handler) *loopbackServer {
	t.Helper()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("loopback listeners are not permitted in this environment: %v", err)
		}
		t.Fatalf("net.Listen() error = %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	loopback := &loopbackServer{
		URL:      "http://" + listener.Addr().String(),
		server:   server,
		listener: listener,
	}
	t.Cleanup(loopback.Close)
	return loopback
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
