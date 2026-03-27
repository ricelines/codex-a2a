package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestRealCodexUsesAuthConfigAndAgentsInstructions(t *testing.T) {
	bin := findRealCodexBinary(t)

	recorder := newResponsesRecorder("real-codex-ok")
	server := recorder.server(t)
	defer server.Close()

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("be precise"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}

	h := newRealCodexHarness(t, realCodexHarnessOptions{
		Binary:            bin,
		Workspace:         workspace,
		ModelProviderBase: server.URL,
		RequireOpenAIAuth: true,
		EnableAgentsMD:    true,
	})

	ctx, cancel := context.WithTimeout(newAuthedContext(), 30*time.Second)
	defer cancel()

	task := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello from real codex"}))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf(
			"task.Status.State = %s, want %s; status message=%q; captured requests=%d",
			task.Status.State,
			a2a.TaskStateInputRequired,
			statusMessageText(task),
			len(recorder.requests()),
		)
	}

	req := recorder.singleRequest(t)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-key" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer sk-test-key")
	}

	texts := req.userInputTexts(t)
	assertContainsText(t, texts, "hello from real codex")
	assertContainsPrefix(t, texts, "# AGENTS.md instructions for ")
	assertContainsText(t, texts, "be precise")
}

func TestRealCodexCreatesDistinctThreadsForFollowUpTasks(t *testing.T) {
	bin := findRealCodexBinary(t)

	recorder := newResponsesRecorder("thread-check")
	server := recorder.server(t)
	defer server.Close()

	h := newRealCodexHarness(t, realCodexHarnessOptions{
		Binary:            bin,
		Workspace:         t.TempDir(),
		ModelProviderBase: server.URL,
		RequireOpenAIAuth: true,
	})

	ctx, cancel := context.WithTimeout(newAuthedContext(), 30*time.Second)
	defer cancel()

	first := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "first task"}))
	if first.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("first task status = %s; message=%q", first.Status.State, statusMessageText(first))
	}

	secondMsg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "second task"})
	secondMsg.ContextID = first.ContextID
	second := mustSendTask(ctx, t, h.handler, secondMsg)
	if second.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("second task status = %s; message=%q", second.Status.State, statusMessageText(second))
	}

	reqs := recorder.requests()
	if len(reqs) != 2 {
		t.Fatalf("captured %d requests, want 2", len(reqs))
	}
	firstSessionID := reqs[0].Header.Get("session_id")
	secondSessionID := reqs[1].Header.Get("session_id")
	if firstSessionID == "" || secondSessionID == "" {
		t.Fatalf("session_id headers missing: first=%q second=%q", firstSessionID, secondSessionID)
	}
	if firstSessionID == secondSessionID {
		t.Fatalf("follow-up request reused session_id %q", firstSessionID)
	}
}

func TestRealCodexUsesTrustedResponsesProxyWithChatGPTAuth(t *testing.T) {
	bin := findRealCodexBinary(t)

	helperCfg := DefaultConfig()
	helperCfg.DefaultCwd = t.TempDir()
	helperCfg.CodexAppServerBin = os.Args[0]
	helperCfg.CodexCLI = ""
	helperCfg.CodexArgs = []string{"-test.run=TestFakeCodexHelperProcess", "--"}
	helperCfg.ChildEnv = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"FAKE_CODEX_AUTH_TOKEN_INITIAL=" + makeChatGPTAccessToken(t, "org-initial"),
		"FAKE_CODEX_AUTH_TOKEN_REFRESHED=" + makeChatGPTAccessToken(t, "org-refreshed"),
	}

	var mu sync.Mutex
	var authHeaders []string
	var accountHeaders []string
	upstream := newLoopbackHTTPServer(t, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		accountHeaders = append(accountHeaders, req.Header.Get("ChatGPT-Account-Id"))
		call := len(authHeaders)
		mu.Unlock()

		if req.Method != http.MethodPost || req.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream got %s %s, want POST /backend-api/codex/responses", req.Method, req.URL.Path)
		}
		if call == 1 {
			http.Error(rw, "expired", http.StatusUnauthorized)
			return
		}

		rw.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(rw, fmt.Sprintf(
			"event: response.created\n"+
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"+
				"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"proxied real codex\"}]}}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"proxied real codex\"}]}]}}\n\n",
		))
	}))
	defer upstream.Close()

	authProxyHandler, authProxyCloser, err := NewCodexAuthProxyHandler(
		context.Background(),
		helperCfg,
		"http://api.invalid/v1/responses",
		upstream.URL+"/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("NewCodexAuthProxyHandler() error = %v", err)
	}
	defer func() {
		if err := authProxyCloser.Close(); err != nil {
			t.Fatalf("authProxyCloser.Close() error = %v", err)
		}
	}()
	authProxy := newLoopbackHTTPServer(t, authProxyHandler)

	h := newRealCodexHarness(t, realCodexHarnessOptions{
		Binary:              bin,
		Workspace:           t.TempDir(),
		ResponsesAPIBaseURL: authProxy.URL + "/v1",
		RequireOpenAIAuth:   false,
	})

	ctx, cancel := context.WithTimeout(newAuthedContext(), 30*time.Second)
	defer cancel()

	task := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello through trusted proxy"}))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task.Status.State = %s, want %s; status message=%q", task.Status.State, a2a.TaskStateInputRequired, statusMessageText(task))
	}
	assertTaskArtifactContains(t, task, "proxied real codex")

	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) != 2 {
		t.Fatalf("upstream request count = %d, want 2", len(authHeaders))
	}
	if authHeaders[0] != "Bearer "+makeChatGPTAccessToken(t, "org-initial") {
		t.Fatalf("first Authorization header = %q, want initial token", authHeaders[0])
	}
	if authHeaders[1] != "Bearer "+makeChatGPTAccessToken(t, "org-refreshed") {
		t.Fatalf("second Authorization header = %q, want refreshed token", authHeaders[1])
	}
	if accountHeaders[0] != "org-initial" {
		t.Fatalf("first ChatGPT-Account-Id = %q, want %q", accountHeaders[0], "org-initial")
	}
	if accountHeaders[1] != "org-refreshed" {
		t.Fatalf("second ChatGPT-Account-Id = %q, want %q", accountHeaders[1], "org-refreshed")
	}
}

func TestRealCodexUsesTrustedResponsesProxyWithAPIKeyAuth(t *testing.T) {
	bin := findRealCodexBinary(t)

	helperHome := t.TempDir()
	if err := writeMockResponsesConfig(filepath.Join(helperHome, "config.toml"), "", true, false); err != nil {
		t.Fatalf("writeMockResponsesConfig() error = %v", err)
	}
	if err := writeFakeAPIKeyAuth(filepath.Join(helperHome, "auth.json"), "sk-test-key"); err != nil {
		t.Fatalf("writeFakeAPIKeyAuth() error = %v", err)
	}

	helperCfg := DefaultConfig()
	helperCfg.DefaultCwd = t.TempDir()
	helperCfg.CodexCLI = bin
	helperCfg.ChildEnv = []string{"CODEX_HOME=" + helperHome}

	recorder := newResponsesRecorder("proxied api key")
	upstream := recorder.server(t)
	defer upstream.Close()

	authProxyHandler, authProxyCloser, err := NewCodexAuthProxyHandler(
		context.Background(),
		helperCfg,
		upstream.URL+"/v1/responses",
		"http://chatgpt.invalid/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("NewCodexAuthProxyHandler() error = %v", err)
	}
	defer func() {
		if err := authProxyCloser.Close(); err != nil {
			t.Fatalf("authProxyCloser.Close() error = %v", err)
		}
	}()
	authProxy := newLoopbackHTTPServer(t, authProxyHandler)

	h := newRealCodexHarness(t, realCodexHarnessOptions{
		Binary:              bin,
		Workspace:           t.TempDir(),
		ResponsesAPIBaseURL: authProxy.URL + "/v1",
		RequireOpenAIAuth:   false,
	})

	ctx, cancel := context.WithTimeout(newAuthedContext(), 30*time.Second)
	defer cancel()

	task := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello through trusted proxy"}))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task.Status.State = %s, want %s; status message=%q", task.Status.State, a2a.TaskStateInputRequired, statusMessageText(task))
	}
	assertTaskArtifactContains(t, task, "proxied api key")

	req := recorder.singleRequest(t)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-key" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer sk-test-key")
	}
	if got := req.Header.Get("ChatGPT-Account-Id"); got != "" {
		t.Fatalf("ChatGPT-Account-Id = %q, want empty for API-key auth", got)
	}
}

type realCodexHarnessOptions struct {
	Binary              string
	Workspace           string
	ModelProviderBase   string
	ResponsesAPIBaseURL string
	RequireOpenAIAuth   bool
	EnableAgentsMD      bool
}

func newRealCodexHarness(t *testing.T, opts realCodexHarnessOptions) *testHarness {
	t.Helper()

	codexHome := t.TempDir()
	if err := writeMockResponsesConfig(filepath.Join(codexHome, "config.toml"), opts.ModelProviderBase, opts.RequireOpenAIAuth, opts.EnableAgentsMD); err != nil {
		t.Fatalf("writeMockResponsesConfig() error = %v", err)
	}
	if opts.RequireOpenAIAuth {
		if err := writeFakeAPIKeyAuth(filepath.Join(codexHome, "auth.json"), "sk-test-key"); err != nil {
			t.Fatalf("writeFakeAPIKeyAuth() error = %v", err)
		}
	}

	cfg := DefaultConfig()
	cfg.DefaultCwd = opts.Workspace
	cfg.CodexCLI = opts.Binary
	cfg.ResponsesAPIBaseURL = opts.ResponsesAPIBaseURL
	cfg.ChildEnv = []string{"CODEX_HOME=" + codexHome}

	executor, err := NewExecutor(cfg)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	t.Cleanup(func() {
		if err := executor.Close(); err != nil {
			t.Fatalf("executor.Close() error = %v", err)
		}
	})
	return &testHarness{
		handler:  NewHandler(executor),
		executor: executor,
	}
}

func findRealCodexBinary(t *testing.T) string {
	t.Helper()

	if path := os.Getenv("CODEX_A2A_REAL_CODEX_BIN"); path != "" {
		return path
	}
	local := filepath.Join("local", "codex", "codex-rs", "target", "debug", "codex")
	if info, err := os.Stat(local); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return local
	}
	path, err := exec.LookPath("codex")
	if err == nil {
		return path
	}
	t.Skip("real codex binary not found; set CODEX_A2A_REAL_CODEX_BIN or install codex")
	return ""
}

func writeMockResponsesConfig(path, serverURL string, requireOpenAIAuth, enableAgentsMD bool) error {
	featureBlock := ""
	if enableAgentsMD {
		featureBlock = "child_agents_md = true\n"
	}
	providerSelection := ""
	providerBlock := ""
	if serverURL != "" {
		requiresLine := ""
		if requireOpenAIAuth {
			requiresLine = "requires_openai_auth = true\n"
		}
		providerSelection = "model_provider = \"mock_provider\"\n"
		providerBlock = fmt.Sprintf(`
[model_providers.mock_provider]
name = "Mock provider for test"
base_url = "%s/v1"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
%s
`, serverURL, requiresLine)
	}
	config := fmt.Sprintf(`
model = "mock-model"
approval_policy = "never"
sandbox_mode = "read-only"
enable_request_compression = false
%s

[features]
%s
%s
`, providerSelection, featureBlock, providerBlock)
	return os.WriteFile(path, []byte(strings.TrimSpace(config)+"\n"), 0o644)
}

func writeFakeAPIKeyAuth(path, apiKey string) error {
	auth := map[string]any{
		"OPENAI_API_KEY": apiKey,
		"last_refresh":   "2026-01-01T00:00:00Z",
	}
	blob, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o600)
}

type responsesRecorder struct {
	mu       sync.Mutex
	response string
	reqs     []capturedResponseRequest
}

type capturedResponseRequest struct {
	Header http.Header
	Body   []byte
}

func newResponsesRecorder(response string) *responsesRecorder {
	return &responsesRecorder{response: response}
}

type loopbackServer struct {
	URL      string
	server   *http.Server
	listener net.Listener
}

func (s *loopbackServer) Close() {
	_ = s.server.Close()
	_ = s.listener.Close()
}

func (r *responsesRecorder) server(t *testing.T) *loopbackServer {
	t.Helper()

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/responses" {
			http.NotFound(rw, req)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, capturedResponseRequest{
			Header: req.Header.Clone(),
			Body:   body,
		})
		r.mu.Unlock()

		rw.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(rw, fmt.Sprintf(
			"event: response.created\n"+
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"+
				"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n",
			r.response,
		))
	})

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sandbox cannot open a loopback listener for real Codex tests: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() {
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
	return &loopbackServer{
		URL:      "http://" + listener.Addr().String(),
		server:   server,
		listener: listener,
	}
}

func (r *responsesRecorder) requests() []capturedResponseRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedResponseRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func (r *responsesRecorder) singleRequest(t *testing.T) capturedResponseRequest {
	t.Helper()

	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("captured %d requests, want 1", len(reqs))
	}
	return reqs[0]
}

func (r capturedResponseRequest) userInputTexts(t *testing.T) []string {
	t.Helper()

	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("Unmarshal(request body) error = %v\nbody=%s", err, string(r.Body))
	}

	var texts []string
	for _, item := range body.Input {
		if item["type"] != "message" || item["role"] != "user" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, raw := range content {
			entry, _ := raw.(map[string]any)
			if entry["type"] == "input_text" {
				if text, _ := entry["text"].(string); text != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	return texts
}

func assertContainsText(t *testing.T, texts []string, want string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, want) {
			return
		}
	}
	t.Fatalf("texts %#v did not include %q", texts, want)
}

func assertContainsPrefix(t *testing.T, texts []string, prefix string) {
	t.Helper()
	for _, text := range texts {
		if strings.HasPrefix(text, prefix) {
			return
		}
	}
	t.Fatalf("texts %#v did not include prefix %q", texts, prefix)
}

func statusMessageText(task *a2a.Task) string {
	if task.Status.Message == nil {
		return ""
	}
	return messageText(task.Status.Message)
}
