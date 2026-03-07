package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	server := recorder.server()
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

	task := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello from real codex")))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf(
			"task.Status.State = %s, want %s; status message=%q; captured requests=%d",
			task.Status.State,
			a2a.TaskStateCompleted,
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
	server := recorder.server()
	defer server.Close()

	h := newRealCodexHarness(t, realCodexHarnessOptions{
		Binary:            bin,
		Workspace:         t.TempDir(),
		ModelProviderBase: server.URL,
		RequireOpenAIAuth: true,
	})

	ctx, cancel := context.WithTimeout(newAuthedContext(), 30*time.Second)
	defer cancel()

	first := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("first task")))
	if first.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("first task status = %s; message=%q", first.Status.State, statusMessageText(first))
	}

	secondMsg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("second task"))
	secondMsg.ContextID = first.ContextID
	second := mustSendTask(ctx, t, h.handler, secondMsg)
	if second.Status.State != a2a.TaskStateCompleted {
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

type realCodexHarnessOptions struct {
	Binary            string
	Workspace         string
	ModelProviderBase string
	RequireOpenAIAuth bool
	EnableAgentsMD    bool
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
	requiresLine := ""
	if requireOpenAIAuth {
		requiresLine = "requires_openai_auth = true\n"
	}
	config := fmt.Sprintf(`
model = "mock-model"
approval_policy = "never"
sandbox_mode = "read-only"
enable_request_compression = false
model_provider = "mock_provider"

[features]
%s

[model_providers.mock_provider]
name = "Mock provider for test"
base_url = "%s/v1"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
%s
`, featureBlock, serverURL, requiresLine)
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

func (r *responsesRecorder) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
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
	}))
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
