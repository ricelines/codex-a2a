package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

func TestExecutorStreamingCompletes(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello"})
	events, err := collectEvents(h.handler.OnSendMessageStream(ctx, &a2a.MessageSendParams{Message: msg}))
	if err != nil {
		t.Fatalf("OnSendMessageStream() error = %v", err)
	}

	assertHasTaskState(t, events, a2a.TaskStateSubmitted)
	assertHasTaskState(t, events, a2a.TaskStateWorking)
	assertHasArtifactText(t, events, "done: hello")
	assertHasTaskState(t, events, a2a.TaskStateCompleted)
}

func TestExecutorApprovalRoundTrip(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "NEEDS_APPROVAL"})
	firstRun, err := collectEvents(h.handler.OnSendMessageStream(ctx, &a2a.MessageSendParams{Message: msg}))
	if err != nil {
		t.Fatalf("first OnSendMessageStream() error = %v", err)
	}
	assertHasTaskState(t, firstRun, a2a.TaskStateInputRequired)

	taskID, contextID := taskIdentity(t, firstRun)
	reply := a2a.NewMessageForTask(
		a2a.MessageRoleUser,
		a2a.TaskInfo{TaskID: taskID, ContextID: contextID},
		a2a.DataPart{Data: map[string]any{"decision": "accept"}},
	)
	secondRun, err := collectEvents(h.handler.OnSendMessageStream(ctx, &a2a.MessageSendParams{Message: reply}))
	if err != nil {
		t.Fatalf("second OnSendMessageStream() error = %v", err)
	}
	assertHasTaskState(t, secondRun, a2a.TaskStateWorking)
	assertHasArtifactText(t, secondRun, "approval accepted")
	assertHasTaskState(t, secondRun, a2a.TaskStateCompleted)
}

func TestExecutorCancelTask(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "NEEDS_APPROVAL"})
	firstRun, err := collectEvents(h.handler.OnSendMessageStream(ctx, &a2a.MessageSendParams{Message: msg}))
	if err != nil {
		t.Fatalf("OnSendMessageStream() error = %v", err)
	}
	assertHasTaskState(t, firstRun, a2a.TaskStateInputRequired)

	taskID, _ := taskIdentity(t, firstRun)
	result, err := h.handler.OnCancelTask(ctx, &a2a.TaskIDParams{ID: taskID})
	if err != nil {
		t.Fatalf("OnCancelTask() error = %v", err)
	}
	if result.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("OnCancelTask() state = %s, want %s", result.Status.State, a2a.TaskStateCanceled)
	}
}

func TestExecutorListTasksByContext(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	firstTask := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "first task"}))

	second := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "second task"})
	second.ContextID = firstTask.ContextID
	secondTask := mustSendTask(ctx, t, h.handler, second)

	list, err := h.handler.OnListTasks(ctx, &a2a.ListTasksRequest{ContextID: firstTask.ContextID, IncludeArtifacts: true})
	if err != nil {
		t.Fatalf("OnListTasks() error = %v", err)
	}
	if len(list.Tasks) != 2 {
		t.Fatalf("OnListTasks() returned %d tasks, want 2", len(list.Tasks))
	}
	if list.Tasks[0].ContextID != firstTask.ContextID || list.Tasks[1].ContextID != secondTask.ContextID {
		t.Fatalf("OnListTasks() returned tasks from unexpected contexts: %#v", list.Tasks)
	}
}

func TestExecutorInvalidInitialMessageDoesNotCreateForkParent(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	invalid := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: ""}))
	if invalid.Status.State != a2a.TaskStateFailed {
		t.Fatalf("invalid.Status.State = %s, want %s", invalid.Status.State, a2a.TaskStateFailed)
	}
	if invalid.Status.Message == nil || !strings.Contains(messageText(invalid.Status.Message), "Codex-compatible") {
		t.Fatalf("invalid.Status.Message = %#v, want Codex-compatible error", invalid.Status.Message)
	}

	before := fakeOperations(t, h.stateDir)
	if len(before) != 0 {
		t.Fatalf("invalid first message unexpectedly touched codex state: %#v", before)
	}

	retry := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello"})
	retry.ContextID = invalid.ContextID
	task := mustSendTask(ctx, t, h.handler, retry)

	assertTaskArtifactContains(t, task, "done: hello")

	var starts, forks int
	for _, op := range fakeOperations(t, h.stateDir) {
		switch op.Method {
		case "thread/start":
			starts++
		case "thread/fork":
			forks++
		}
	}
	if starts != 1 {
		t.Fatalf("thread/start count = %d, want 1", starts)
	}
	if forks != 0 {
		t.Fatalf("thread/fork count = %d, want 0", forks)
	}
}

type testHarness struct {
	handler  a2asrv.RequestHandler
	executor *Executor
	stateDir string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	cfg := DefaultConfig()
	cfg.DefaultCwd = t.TempDir()
	cfg.CodexAppServerBin = os.Args[0]
	cfg.CodexCLI = ""
	cfg.CodexArgs = []string{"-test.run=TestFakeCodexHelperProcess", "--"}

	stateDir := t.TempDir()
	cfg.ChildEnv = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"FAKE_CODEX_STATE_DIR=" + stateDir,
	}

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
		stateDir: stateDir,
	}
}

func newTestHandler(t *testing.T) a2asrv.RequestHandler {
	t.Helper()
	return newTestHarness(t).handler
}

func newAuthedContext() context.Context {
	ctx, callCtx := a2asrv.WithCallContext(context.Background(), nil)
	callCtx.User = &a2asrv.AuthenticatedUser{UserName: "tester"}
	return ctx
}

func mustSendTask(ctx context.Context, t *testing.T, handler a2asrv.RequestHandler, msg *a2a.Message) *a2a.Task {
	t.Helper()
	result, err := handler.OnSendMessage(ctx, &a2a.MessageSendParams{Message: msg})
	if err != nil {
		t.Fatalf("OnSendMessage() error = %v", err)
	}
	task, ok := result.(*a2a.Task)
	if !ok {
		t.Fatalf("OnSendMessage() result = %T, want *a2a.Task", result)
	}
	return task
}

func collectEvents(seq func(func(a2a.Event, error) bool)) ([]a2a.Event, error) {
	events := make([]a2a.Event, 0, 8)
	var streamErr error
	seq(func(event a2a.Event, err error) bool {
		if err != nil {
			streamErr = err
			return false
		}
		events = append(events, event)
		return true
	})
	return events, streamErr
}

func assertHasTaskState(t *testing.T, events []a2a.Event, want a2a.TaskState) {
	t.Helper()
	for _, event := range events {
		switch v := event.(type) {
		case *a2a.Task:
			if v.Status.State == want {
				return
			}
		case *a2a.TaskStatusUpdateEvent:
			if v.Status.State == want {
				return
			}
		}
	}
	t.Fatalf("events did not include task state %s: %#v", want, events)
}

func assertHasArtifactText(t *testing.T, events []a2a.Event, want string) {
	t.Helper()
	for _, event := range events {
		artifact, ok := event.(*a2a.TaskArtifactUpdateEvent)
		if !ok {
			continue
		}
		for _, part := range artifact.Artifact.Parts {
			if strings.Contains(partText(part), want) {
				return
			}
		}
	}
	t.Fatalf("events did not include artifact text %q: %#v", want, events)
}

func taskIdentity(t *testing.T, events []a2a.Event) (a2a.TaskID, string) {
	t.Helper()
	for _, event := range events {
		if task, ok := event.(*a2a.Task); ok {
			return task.ID, task.ContextID
		}
	}
	t.Fatalf("no task event found")
	return "", ""
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

type fakeThreadState struct {
	ID      string   `json:"id"`
	History []string `json:"history,omitempty"`
}

type fakeOperation struct {
	Method         string `json:"method"`
	ThreadID       string `json:"threadId,omitempty"`
	ParentThreadID string `json:"parentThreadId,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	PID            int    `json:"pid"`
}

func fakeOperations(t *testing.T, stateDir string) []fakeOperation {
	t.Helper()

	dir := filepath.Join(stateDir, "ops")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	ops := make([]fakeOperation, 0, len(entries))
	for _, entry := range entries {
		blob, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", entry.Name(), err)
		}
		var op fakeOperation
		if err := json.Unmarshal(blob, &op); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", entry.Name(), err)
		}
		ops = append(ops, op)
	}
	return ops
}

func countActiveFakeCodexProcesses(t *testing.T, stateDir string) int {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(stateDir, "processes"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("ReadDir(processes) error = %v", err)
	}
	return len(entries)
}

func assertTaskArtifactContains(t *testing.T, task *a2a.Task, want string) {
	t.Helper()
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			if strings.Contains(partText(part), want) {
				return
			}
		}
	}
	t.Fatalf("task artifacts did not include %q: %#v", want, task.Artifacts)
}

func taskMetaValue(task *a2a.Task, key string) any {
	if task.Metadata == nil {
		return nil
	}
	meta, _ := task.Metadata[metadataNamespace].(map[string]any)
	return meta[key]
}

func TestFakeCodexHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("helper process")
	}
	if err := runFakeCodex(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runFakeCodex(stdin io.Reader, stdout io.Writer) error {
	server := &fakeCodexServer{
		reader:   bufio.NewReader(stdin),
		writer:   json.NewEncoder(stdout),
		stateDir: os.Getenv("FAKE_CODEX_STATE_DIR"),
	}
	return server.run()
}

type fakeCodexServer struct {
	reader *bufio.Reader
	writer *json.Encoder

	stateDir string

	turnCount    int
	requestCount int
	opCount      int

	active *fakeActiveTurn
}

type fakeActiveTurn struct {
	threadID string
	turnID   string
	prompt   string
	history  []string

	itemID    string
	requestID int
	mode      string
}

func (s *fakeCodexServer) run() error {
	if err := s.markProcessAlive(); err != nil {
		return err
	}
	defer s.markProcessStopped()

	for {
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 {
			continue
		}

		var env rpcEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			return err
		}

		if env.Method != "" {
			if len(env.ID) > 0 {
				if err := s.handleRequest(env); err != nil {
					return err
				}
			}
			continue
		}
		if len(env.ID) > 0 {
			if err := s.handleResponse(env); err != nil {
				return err
			}
		}
	}
}

func (s *fakeCodexServer) markProcessAlive() error {
	if s.stateDir == "" {
		return nil
	}
	dir := filepath.Join(s.stateDir, "processes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.alive", os.Getpid())), []byte("alive"), 0o644)
}

func (s *fakeCodexServer) markProcessStopped() {
	if s.stateDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(s.stateDir, "processes", fmt.Sprintf("%d.alive", os.Getpid())))
}

func (s *fakeCodexServer) handleRequest(env rpcEnvelope) error {
	switch env.Method {
	case "initialize":
		return s.write(map[string]any{
			"id":     rawID(env.ID),
			"result": map[string]any{"userAgent": "fake-codex/0.2"},
		})
	case "thread/start":
		thread := fakeThreadState{ID: s.newID("thr")}
		if err := s.saveThread(thread); err != nil {
			return err
		}
		if err := s.logOperation(fakeOperation{Method: "thread/start", ThreadID: thread.ID, PID: os.Getpid()}); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"id":     rawID(env.ID),
			"result": map[string]any{"thread": map[string]any{"id": thread.ID}},
		}); err != nil {
			return err
		}
		return s.write(map[string]any{
			"method": "thread/started",
			"params": map[string]any{"thread": map[string]any{"id": thread.ID}},
		})
	case "thread/resume":
		var params struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(env.Params, &params); err != nil {
			return err
		}
		if _, err := s.loadThread(params.ThreadID); err != nil {
			return err
		}
		if err := s.logOperation(fakeOperation{Method: "thread/resume", ThreadID: params.ThreadID, PID: os.Getpid()}); err != nil {
			return err
		}
		return s.write(map[string]any{
			"id":     rawID(env.ID),
			"result": map[string]any{"thread": map[string]any{"id": params.ThreadID}},
		})
	case "thread/fork":
		var params struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(env.Params, &params); err != nil {
			return err
		}
		parent, err := s.loadThread(params.ThreadID)
		if err != nil {
			return err
		}
		child := fakeThreadState{
			ID:      s.newID("thr"),
			History: append([]string(nil), parent.History...),
		}
		if err := s.saveThread(child); err != nil {
			return err
		}
		if err := s.logOperation(fakeOperation{
			Method:         "thread/fork",
			ThreadID:       child.ID,
			ParentThreadID: parent.ID,
			PID:            os.Getpid(),
		}); err != nil {
			return err
		}
		return s.write(map[string]any{
			"id":     rawID(env.ID),
			"result": map[string]any{"thread": map[string]any{"id": child.ID}},
		})
	case "turn/start":
		return s.handleTurnStart(env)
	case "turn/interrupt":
		if err := s.write(map[string]any{"id": rawID(env.ID), "result": map[string]any{}}); err != nil {
			return err
		}
		if s.active == nil {
			return nil
		}
		return s.write(map[string]any{
			"method": "turn/completed",
			"params": map[string]any{
				"threadId": s.active.threadID,
				"turn": map[string]any{
					"id":     s.active.turnID,
					"status": "interrupted",
				},
			},
		})
	default:
		return s.write(map[string]any{
			"id":    rawID(env.ID),
			"error": map[string]any{"code": -32601, "message": "method not found"},
		})
	}
}

func (s *fakeCodexServer) handleTurnStart(env rpcEnvelope) error {
	var params struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"input"`
	}
	if err := json.Unmarshal(env.Params, &params); err != nil {
		return err
	}

	thread, err := s.loadThread(params.ThreadID)
	if err != nil {
		return err
	}

	s.turnCount++
	turnID := fmt.Sprintf("turn-%d-%d", os.Getpid(), s.turnCount)
	prompt := ""
	for _, input := range params.Input {
		if input.Type == "text" {
			prompt = strings.TrimSpace(input.Text)
			break
		}
	}

	s.active = &fakeActiveTurn{
		threadID: params.ThreadID,
		turnID:   turnID,
		prompt:   prompt,
		history:  append([]string(nil), thread.History...),
	}
	if err := s.logOperation(fakeOperation{
		Method:   "turn/start",
		ThreadID: params.ThreadID,
		Prompt:   prompt,
		PID:      os.Getpid(),
	}); err != nil {
		return err
	}

	if err := s.write(map[string]any{
		"id": rawID(env.ID),
		"result": map[string]any{
			"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		},
	}); err != nil {
		return err
	}
	if err := s.write(map[string]any{
		"method": "turn/started",
		"params": map[string]any{
			"threadId": params.ThreadID,
			"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		},
	}); err != nil {
		return err
	}

	switch prompt {
	case "NEEDS_APPROVAL":
		s.requestCount++
		s.active.mode = "approval"
		s.active.requestID = s.requestCount
		s.active.itemID = fmt.Sprintf("cmd-%d", s.requestCount)
		if err := s.write(map[string]any{
			"method": "item/started",
			"params": map[string]any{
				"threadId": params.ThreadID,
				"turnId":   turnID,
				"item": map[string]any{
					"type":           "commandExecution",
					"id":             s.active.itemID,
					"command":        "echo approved",
					"cwd":            "/workspace",
					"status":         "inProgress",
					"commandActions": []any{},
				},
			},
		}); err != nil {
			return err
		}
		return s.write(map[string]any{
			"id":     s.active.requestID,
			"method": "item/commandExecution/requestApproval",
			"params": map[string]any{
				"threadId": params.ThreadID,
				"turnId":   turnID,
				"itemId":   s.active.itemID,
				"command":  "echo approved",
				"cwd":      "/workspace",
				"reason":   "needs approval",
			},
		})
	case "NEEDS_FILE_APPROVAL":
		s.requestCount++
		s.active.mode = "fileApproval"
		s.active.requestID = s.requestCount
		s.active.itemID = fmt.Sprintf("file-%d", s.requestCount)
		if err := s.write(map[string]any{
			"method": "item/started",
			"params": map[string]any{
				"threadId": params.ThreadID,
				"turnId":   turnID,
				"item": map[string]any{
					"type":   "fileChange",
					"id":     s.active.itemID,
					"status": "inProgress",
					"changes": []map[string]any{{
						"path": "hello.txt",
						"kind": "create",
						"diff": "+hello\n",
					}},
				},
			},
		}); err != nil {
			return err
		}
		return s.write(map[string]any{
			"id":     s.active.requestID,
			"method": "item/fileChange/requestApproval",
			"params": map[string]any{
				"threadId": params.ThreadID,
				"turnId":   turnID,
				"itemId":   s.active.itemID,
				"reason":   "needs file approval",
			},
		})
	case "NEEDS_ELICITATION":
		s.requestCount++
		s.active.mode = "elicitation"
		s.active.requestID = s.requestCount
		s.active.itemID = fmt.Sprintf("mcp-%d", s.requestCount)
		return s.write(map[string]any{
			"id":     s.active.requestID,
			"method": "mcpServer/elicitation/request",
			"params": map[string]any{
				"threadId":        params.ThreadID,
				"turnId":          turnID,
				"serverName":      "test-mcp",
				"mode":            "structured",
				"message":         "Please provide structured input.",
				"requestedSchema": map[string]any{"type": "object"},
			},
		})
	case "CANCEL_ME":
		s.active.mode = "cancel"
		return nil
	case "WAIT_FOREVER":
		s.active.mode = "wait"
		return nil
	case "FAIL_TURN":
		return s.write(map[string]any{
			"method": "turn/completed",
			"params": map[string]any{
				"threadId": params.ThreadID,
				"turn": map[string]any{
					"id":     turnID,
					"status": "failed",
					"error":  map[string]any{"message": "synthetic failure"},
				},
			},
		})
	default:
		return s.completeTurn(params.ThreadID, turnID, prompt, thread.History, fakeTurnOutput(thread.History, prompt))
	}
}

func fakeTurnOutput(history []string, prompt string) string {
	switch {
	case prompt == "WHAT_DO_YOU_REMEMBER":
		if len(history) == 0 {
			return "(empty)"
		}
		return strings.Join(history, " | ")
	case strings.HasPrefix(prompt, "REMEMBER "):
		return "remembered: " + strings.TrimPrefix(prompt, "REMEMBER ")
	default:
		return "done: " + prompt
	}
}

func (s *fakeCodexServer) handleResponse(env rpcEnvelope) error {
	if s.active == nil {
		return nil
	}
	if string(env.ID) != fmt.Sprintf("%d", s.active.requestID) {
		return nil
	}

	switch s.active.mode {
	case "approval":
		var result struct {
			Decision any `json:"decision"`
		}
		if err := json.Unmarshal(env.Result, &result); err != nil {
			return err
		}
		decision := fmt.Sprint(result.Decision)
		output := "approval accepted"
		status := "completed"
		if decision == "decline" {
			output = "approval declined"
			status = "declined"
		}
		if err := s.write(map[string]any{
			"method": "serverRequest/resolved",
			"params": map[string]any{
				"threadId":  s.active.threadID,
				"requestId": s.active.requestID,
			},
		}); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": s.active.threadID,
				"turnId":   s.active.turnID,
				"item": map[string]any{
					"type":             "commandExecution",
					"id":               s.active.itemID,
					"command":          "echo approved",
					"cwd":              "/workspace",
					"status":           status,
					"commandActions":   []any{},
					"aggregatedOutput": output,
					"exitCode":         0,
				},
			},
		}); err != nil {
			return err
		}
		return s.completeTurn(s.active.threadID, s.active.turnID, s.active.prompt, s.active.history, output)
	case "fileApproval":
		var result struct {
			Decision any `json:"decision"`
		}
		if err := json.Unmarshal(env.Result, &result); err != nil {
			return err
		}
		diff := "--- /dev/null\n+++ hello.txt\n@@\n+hello\n"
		if err := s.write(map[string]any{
			"method": "serverRequest/resolved",
			"params": map[string]any{
				"threadId":  s.active.threadID,
				"requestId": s.active.requestID,
			},
		}); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"method": "item/fileChange/outputDelta",
			"params": map[string]any{
				"threadId": s.active.threadID,
				"turnId":   s.active.turnID,
				"itemId":   s.active.itemID,
				"delta":    diff,
			},
		}); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": s.active.threadID,
				"turnId":   s.active.turnID,
				"item": map[string]any{
					"type":   "fileChange",
					"id":     s.active.itemID,
					"status": fmt.Sprint(result.Decision),
					"changes": []map[string]any{{
						"path": "hello.txt",
						"kind": "create",
						"diff": diff,
					}},
				},
			},
		}); err != nil {
			return err
		}
		return s.completeTurn(s.active.threadID, s.active.turnID, s.active.prompt, s.active.history, "file approval handled")
	case "elicitation":
		var result map[string]any
		if err := json.Unmarshal(env.Result, &result); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"method": "serverRequest/resolved",
			"params": map[string]any{
				"threadId":  s.active.threadID,
				"requestId": s.active.requestID,
			},
		}); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": s.active.threadID,
				"turnId":   s.active.turnID,
				"item": map[string]any{
					"type":   "mcpToolCall",
					"id":     s.active.itemID,
					"server": "test-mcp",
					"tool":   "collect",
					"result": result,
				},
			},
		}); err != nil {
			return err
		}
		return s.completeTurn(s.active.threadID, s.active.turnID, s.active.prompt, s.active.history, "elicitation accepted")
	default:
		return nil
	}
}

func (s *fakeCodexServer) completeTurn(threadID, turnID, prompt string, history []string, text string) error {
	if err := s.saveThread(fakeThreadState{
		ID:      threadID,
		History: append(append([]string(nil), history...), prompt),
	}); err != nil {
		return err
	}

	itemID := fmt.Sprintf("assistant-%s", turnID)
	if err := s.write(map[string]any{
		"method": "turn/plan/updated",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"plan": []map[string]any{
				{"step": "Respond to the user", "status": "completed"},
			},
		},
	}); err != nil {
		return err
	}
	if err := s.write(map[string]any{
		"method": "item/started",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item": map[string]any{
				"type": "agentMessage",
				"id":   itemID,
				"text": "",
			},
		},
	}); err != nil {
		return err
	}
	if err := s.write(map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"itemId":   itemID,
			"delta":    text,
		},
	}); err != nil {
		return err
	}
	if err := s.write(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item": map[string]any{
				"type": "agentMessage",
				"id":   itemID,
				"text": text,
			},
		},
	}); err != nil {
		return err
	}
	s.active = nil
	return s.write(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turn": map[string]any{
				"id":     turnID,
				"status": "completed",
			},
		},
	})
}

func (s *fakeCodexServer) saveThread(thread fakeThreadState) error {
	if s.stateDir == "" {
		return nil
	}
	dir := filepath.Join(s.stateDir, "threads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	blob, err := json.Marshal(thread)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, thread.ID+".json"), blob, 0o644)
}

func (s *fakeCodexServer) loadThread(threadID string) (fakeThreadState, error) {
	if s.stateDir == "" {
		return fakeThreadState{ID: threadID}, nil
	}
	blob, err := os.ReadFile(filepath.Join(s.stateDir, "threads", threadID+".json"))
	if err != nil {
		return fakeThreadState{}, err
	}
	var thread fakeThreadState
	if err := json.Unmarshal(blob, &thread); err != nil {
		return fakeThreadState{}, err
	}
	return thread, nil
}

func (s *fakeCodexServer) logOperation(op fakeOperation) error {
	if s.stateDir == "" {
		return nil
	}
	dir := filepath.Join(s.stateDir, "ops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.opCount++
	blob, err := json.Marshal(op)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%06d-%06d.json", time.Now().UnixNano(), os.Getpid(), s.opCount)
	return os.WriteFile(filepath.Join(dir, name), blob, 0o644)
}

func (s *fakeCodexServer) newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())
}

func (s *fakeCodexServer) write(payload any) error {
	return s.writer.Encode(payload)
}

func rawID(raw json.RawMessage) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}
