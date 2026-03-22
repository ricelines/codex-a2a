package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

var authProxyHopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type authTokenProvider interface {
	CurrentAuth(context.Context, bool) (proxyAuth, error)
	Close() error
}

type proxyAuthMethod string

const (
	proxyAuthMethodAPIKey            proxyAuthMethod = "apikey"
	proxyAuthMethodChatGPT           proxyAuthMethod = "chatgpt"
	proxyAuthMethodChatGPTAuthTokens proxyAuthMethod = "chatgptAuthTokens"
)

type proxyAuth struct {
	Method      proxyAuthMethod
	BearerToken string
	AccountID   string
}

func parseProxyAuthMethod(raw string) (proxyAuthMethod, error) {
	switch strings.TrimSpace(raw) {
	case string(proxyAuthMethodAPIKey):
		return proxyAuthMethodAPIKey, nil
	case string(proxyAuthMethodChatGPT):
		return proxyAuthMethodChatGPT, nil
	case string(proxyAuthMethodChatGPTAuthTokens):
		return proxyAuthMethodChatGPTAuthTokens, nil
	default:
		return "", fmt.Errorf("unsupported auth method %q", raw)
	}
}

func (m proxyAuthMethod) needsAccountID() bool {
	switch m {
	case proxyAuthMethodChatGPT, proxyAuthMethodChatGPTAuthTokens:
		return true
	case proxyAuthMethodAPIKey:
		return false
	default:
		return false
	}
}

func (m proxyAuthMethod) retriesUnauthorized() bool {
	switch m {
	case proxyAuthMethodChatGPT, proxyAuthMethodChatGPTAuthTokens:
		return true
	case proxyAuthMethodAPIKey:
		return false
	default:
		return false
	}
}

type codexAuthTokenProvider struct {
	client *codexClient

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func newCodexAuthTokenProvider(ctx context.Context, cfg Config) (*codexAuthTokenProvider, error) {
	client, err := launchCodexClient(ctx, cfg)
	if err != nil {
		return nil, err
	}

	drainCtx, cancel := context.WithCancel(context.Background())
	p := &codexAuthTokenProvider{
		client: client,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go p.drainNotifications(drainCtx)
	return p, nil
}

func (p *codexAuthTokenProvider) drainNotifications(ctx context.Context) {
	defer close(p.done)
	for {
		if _, err := p.client.next(ctx); err != nil {
			return
		}
	}
}

func (p *codexAuthTokenProvider) CurrentAuth(ctx context.Context, refresh bool) (proxyAuth, error) {
	includeToken := true
	respRaw, err := p.client.request(ctx, "getAuthStatus", getAuthStatusParams{
		IncludeToken: &includeToken,
		RefreshToken: &refresh,
	})
	if err != nil {
		return proxyAuth{}, fmt.Errorf("get auth status: %w", err)
	}

	resp, err := decodeJSON[getAuthStatusResponse](respRaw)
	if err != nil {
		return proxyAuth{}, fmt.Errorf("decode auth status: %w", err)
	}
	if resp.RequiresOpenAIAuth != nil && !*resp.RequiresOpenAIAuth {
		return proxyAuth{}, fmt.Errorf("codex helper reported auth is not required")
	}
	if resp.AuthMethod == nil || strings.TrimSpace(*resp.AuthMethod) == "" {
		return proxyAuth{}, fmt.Errorf("codex helper did not report an auth method")
	}
	if resp.AuthToken == nil || strings.TrimSpace(*resp.AuthToken) == "" {
		return proxyAuth{}, fmt.Errorf("codex helper did not return an auth token")
	}

	method, err := parseProxyAuthMethod(*resp.AuthMethod)
	if err != nil {
		return proxyAuth{}, err
	}
	auth := proxyAuth{
		Method:      method,
		BearerToken: strings.TrimSpace(*resp.AuthToken),
	}
	if !method.needsAccountID() {
		return auth, nil
	}

	accountID, err := chatGPTAccountIDFromAccessToken(auth.BearerToken)
	if err != nil {
		return proxyAuth{}, err
	}
	auth.AccountID = accountID
	return auth, nil
}

func (p *codexAuthTokenProvider) Close() error {
	var err error
	p.once.Do(func() {
		p.cancel()
		<-p.done
		err = p.client.Close()
	})
	return err
}

type authProxyHandler struct {
	apiKeyUpstreamURL  *url.URL
	chatGPTUpstreamURL *url.URL
	httpClient         *http.Client
	auth               authTokenProvider
}

func parseAuthProxyUpstream(name string, rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s upstream URL: %w", name, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%s upstream URL must include scheme and host", name)
	}
	return parsed, nil
}

func NewAuthProxyHandler(apiKeyUpstreamURL string, chatGPTUpstreamURL string, auth authTokenProvider) (http.Handler, error) {
	if auth == nil {
		return nil, fmt.Errorf("auth token provider is required")
	}
	apiKeyURL, err := parseAuthProxyUpstream("API key", apiKeyUpstreamURL)
	if err != nil {
		return nil, err
	}
	chatGPTURL, err := parseAuthProxyUpstream("ChatGPT", chatGPTUpstreamURL)
	if err != nil {
		return nil, err
	}
	return &authProxyHandler{
		apiKeyUpstreamURL:  apiKeyURL,
		chatGPTUpstreamURL: chatGPTURL,
		httpClient:         &http.Client{},
		auth:               auth,
	}, nil
}

func NewCodexAuthProxyHandler(
	ctx context.Context,
	cfg Config,
	apiKeyUpstreamURL string,
	chatGPTUpstreamURL string,
) (http.Handler, io.Closer, error) {
	provider, err := newCodexAuthTokenProvider(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	handler, err := NewAuthProxyHandler(apiKeyUpstreamURL, chatGPTUpstreamURL, provider)
	if err != nil {
		_ = provider.Close()
		return nil, nil, err
	}
	return handler, provider, nil
}

func (h *authProxyHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/responses" || req.URL.RawQuery != "" {
		http.Error(rw, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(rw, fmt.Sprintf("read request body: %v", err), http.StatusBadRequest)
		return
	}

	resp, err := h.forwardWithRetry(req.Context(), req.Header, body)
	if err != nil {
		http.Error(rw, fmt.Sprintf("forward request: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHTTPHeaders(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)

	writer := io.Writer(rw)
	if flusher, ok := rw.(http.Flusher); ok {
		writer = &flushingWriter{writer: rw, flusher: flusher}
	}
	_, _ = io.Copy(writer, resp.Body)
}

func (h *authProxyHandler) forwardWithRetry(ctx context.Context, incoming http.Header, body []byte) (*http.Response, error) {
	auth, err := h.auth.CurrentAuth(ctx, false)
	if err != nil {
		return nil, err
	}

	resp, err := h.doForward(ctx, incoming, body, auth)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || !auth.Method.retriesUnauthorized() {
		return resp, nil
	}

	_ = resp.Body.Close()

	auth, err = h.auth.CurrentAuth(ctx, true)
	if err != nil {
		return nil, err
	}
	return h.doForward(ctx, incoming, body, auth)
}

func (h *authProxyHandler) doForward(
	ctx context.Context,
	incoming http.Header,
	body []byte,
	auth proxyAuth,
) (*http.Response, error) {
	upstreamURL, err := h.upstreamURLFor(auth.Method)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	req.Header = cloneForwardHeaders(incoming)
	req.Header.Set("Authorization", "Bearer "+auth.BearerToken)
	if auth.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", auth.AccountID)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send upstream request: %w", err)
	}
	return resp, nil
}

func (h *authProxyHandler) upstreamURLFor(method proxyAuthMethod) (*url.URL, error) {
	switch method {
	case proxyAuthMethodAPIKey:
		return h.apiKeyUpstreamURL, nil
	case proxyAuthMethodChatGPT, proxyAuthMethodChatGPTAuthTokens:
		return h.chatGPTUpstreamURL, nil
	default:
		return nil, fmt.Errorf("unsupported auth method %q", method)
	}
}

func cloneForwardHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if _, hopByHop := authProxyHopByHopHeaders[canonical]; hopByHop {
			continue
		}
		if canonical == "Authorization" || canonical == "Chatgpt-Account-Id" || canonical == "Host" {
			continue
		}
		dst[canonical] = append([]string(nil), values...)
	}
	return dst
}

func copyHTTPHeaders(dst http.Header, src http.Header) {
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if _, hopByHop := authProxyHopByHopHeaders[canonical]; hopByHop {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func chatGPTAccountIDFromAccessToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("access token is not a JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}

	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("decode JWT claims: %w", err)
	}
	if claims.Auth.ChatGPTAccountID == "" {
		return "", fmt.Errorf("access token does not include chatgpt_account_id")
	}
	return claims.Auth.ChatGPTAccountID, nil
}

type flushingWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (w *flushingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.flusher.Flush()
	}
	return n, err
}
