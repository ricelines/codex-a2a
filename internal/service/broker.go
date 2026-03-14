package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/a2aproject/a2a-go/a2a"
)

type pendingKind string

const (
	pendingCommandApproval pendingKind = "commandApproval"
	pendingFileApproval    pendingKind = "fileChangeApproval"
	pendingElicitation     pendingKind = "mcpElicitation"
)

type pendingRequest struct {
	ID   json.RawMessage
	Kind pendingKind

	CommandApproval *commandApprovalRequest
	FileApproval    *fileApprovalRequest
	Elicitation     *elicitationRequest
}

type taskRuntime struct {
	TaskID    a2a.TaskID
	ContextID string
	TurnID    string
	Session   *taskSession

	cancelRequested atomic.Bool

	mu              sync.Mutex
	pending         *pendingRequest
	assistantTexts  map[string]string
	commandOutputs  map[string]string
	fileChangeDiffs map[string]string
}

func newTaskRuntime(taskID a2a.TaskID, contextID string, session *taskSession) *taskRuntime {
	return &taskRuntime{
		TaskID:          taskID,
		ContextID:       contextID,
		Session:         session,
		assistantTexts:  make(map[string]string),
		commandOutputs:  make(map[string]string),
		fileChangeDiffs: make(map[string]string),
	}
}

func (t *taskRuntime) setPending(p *pendingRequest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = p
}

func (t *taskRuntime) pendingRequest() *pendingRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending
}

func (t *taskRuntime) clearPending() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = nil
}

func (t *taskRuntime) appendCommandOutput(itemID string, delta string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commandOutputs[itemID] += delta
	return t.commandOutputs[itemID]
}

func (t *taskRuntime) appendAssistantText(itemID string, delta string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.assistantTexts[itemID] += delta
	return t.assistantTexts[itemID]
}

func (t *taskRuntime) appendFileChangeDiff(itemID string, delta string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fileChangeDiffs[itemID] += delta
	return t.fileChangeDiffs[itemID]
}

func (t *taskRuntime) commandOutput(itemID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.commandOutputs[itemID]
}

func (t *taskRuntime) fileChangeDiff(itemID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fileChangeDiffs[itemID]
}

type taskSession struct {
	TaskID    a2a.TaskID
	ContextID string
	ThreadID  string

	ParentTaskID a2a.TaskID
	Options      RequestOptions
	Client       *codexClient

	childCount int
}

type contextState struct {
	ContextID string
	tasks     map[a2a.TaskID]*taskSession
}

type broker struct {
	cfg Config

	launch func(context.Context, Config) (*codexClient, error)

	mu       sync.Mutex
	contexts map[string]*contextState
	tasks    map[a2a.TaskID]*taskRuntime
}

func newBroker(cfg Config) (*broker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &broker{
		cfg:      cfg,
		launch:   launchCodexClient,
		contexts: make(map[string]*contextState),
		tasks:    make(map[a2a.TaskID]*taskRuntime),
	}, nil
}

func (b *broker) startSession(ctx context.Context, taskID a2a.TaskID, contextID string, options RequestOptions) (*taskSession, error) {
	client, err := b.launch(ctx, b.cfg)
	if err != nil {
		return nil, err
	}
	respRaw, err := client.request(ctx, "thread/start", threadStartParams{
		Cwd:                    options.Cwd,
		Model:                  options.Model,
		ApprovalPolicy:         options.ApprovalPolicy,
		Sandbox:                options.Sandbox,
		Config:                 options.CodexConfig,
		ExperimentalRawEvents:  false,
		PersistExtendedHistory: true,
	})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	resp, err := decodeJSON[threadResponse](respRaw)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("decode thread/start response: %w", err)
	}
	return &taskSession{
		TaskID:    taskID,
		ContextID: contextID,
		ThreadID:  resp.Thread.ID,
		Options:   options,
		Client:    client,
	}, nil
}

func (b *broker) forkSession(
	ctx context.Context,
	taskID a2a.TaskID,
	contextID string,
	parent *taskSession,
	options RequestOptions,
) (*taskSession, error) {
	client, err := b.launch(ctx, b.cfg)
	if err != nil {
		return nil, err
	}
	respRaw, err := client.request(ctx, "thread/fork", threadForkParams{
		ThreadID:               parent.ThreadID,
		Cwd:                    options.Cwd,
		Model:                  options.Model,
		ApprovalPolicy:         options.ApprovalPolicy,
		Sandbox:                options.Sandbox,
		Config:                 options.CodexConfig,
		PersistExtendedHistory: true,
	})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	resp, err := decodeJSON[threadResponse](respRaw)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("decode thread/fork response: %w", err)
	}
	return &taskSession{
		TaskID:       taskID,
		ContextID:    contextID,
		ThreadID:     resp.Thread.ID,
		ParentTaskID: parent.TaskID,
		Options:      options,
		Client:       client,
	}, nil
}

func (b *broker) resolveBaseSession(contextID string, references []a2a.TaskID) (*taskSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	state := b.contexts[contextID]
	if state == nil || len(state.tasks) == 0 {
		if len(references) > 0 {
			return nil, fmt.Errorf("context %s does not contain referenced tasks", contextID)
		}
		return nil, nil
	}

	if len(references) > 1 {
		return nil, fmt.Errorf("context %s has multiple references; supply exactly one referenceTaskId", contextID)
	}
	if len(references) == 1 {
		taskID := references[0]
		session := state.tasks[taskID]
		if session == nil {
			return nil, fmt.Errorf("reference task %s is not part of context %s", taskID, contextID)
		}
		if _, active := b.tasks[taskID]; active {
			return nil, fmt.Errorf("reference task %s is still active; wait for it to finish before branching", taskID)
		}
		return session, nil
	}

	var leaf *taskSession
	for _, session := range state.tasks {
		if session.childCount != 0 {
			continue
		}
		if _, active := b.tasks[session.TaskID]; active {
			return nil, fmt.Errorf("context %s has an active task %s; wait for it to finish or specify referenceTaskIds", contextID, session.TaskID)
		}
		if leaf != nil {
			return nil, fmt.Errorf("context %s has multiple task branches; specify referenceTaskIds", contextID)
		}
		leaf = session
	}
	return leaf, nil
}

func (b *broker) startTask(
	ctx context.Context,
	taskID a2a.TaskID,
	contextID string,
	options RequestOptions,
	references []a2a.TaskID,
) (*taskRuntime, error) {
	parent, err := b.resolveBaseSession(contextID, references)
	if err != nil {
		return nil, err
	}

	var session *taskSession
	switch {
	case parent == nil:
		session, err = b.startSession(ctx, taskID, contextID, options)
	default:
		session, err = b.forkSession(ctx, taskID, contextID, parent, options)
	}
	if err != nil {
		return nil, err
	}

	runtime := newTaskRuntime(taskID, contextID, session)

	b.mu.Lock()
	state := b.contexts[contextID]
	if state == nil {
		state = &contextState{
			ContextID: contextID,
			tasks:     make(map[a2a.TaskID]*taskSession),
		}
		b.contexts[contextID] = state
	}
	if session.ParentTaskID != "" {
		if parent := state.tasks[session.ParentTaskID]; parent != nil {
			parent.childCount++
		}
	}
	state.tasks[taskID] = session
	b.tasks[taskID] = runtime
	b.mu.Unlock()
	return runtime, nil
}

func (b *broker) task(taskID a2a.TaskID) (*taskRuntime, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	runtime := b.tasks[taskID]
	if runtime == nil {
		return nil, fmt.Errorf("task %s is not active in broker state", taskID)
	}
	return runtime, nil
}

func (b *broker) finishTask(taskID a2a.TaskID) {
	b.finishTaskWithPolicy(taskID, true)
}

func (b *broker) discardTask(taskID a2a.TaskID) {
	b.finishTaskWithPolicy(taskID, false)
}

func (b *broker) finishTaskWithPolicy(taskID a2a.TaskID, preserveSession bool) {
	var client *codexClient

	b.mu.Lock()
	runtime := b.tasks[taskID]
	if runtime == nil {
		b.mu.Unlock()
		return
	}
	delete(b.tasks, taskID)
	if runtime.Session != nil {
		if !preserveSession {
			state := b.contexts[runtime.ContextID]
			if state != nil {
				session := state.tasks[taskID]
				if session != nil {
					if session.ParentTaskID != "" {
						if parent := state.tasks[session.ParentTaskID]; parent != nil && parent.childCount > 0 {
							parent.childCount--
						}
					}
					delete(state.tasks, taskID)
					if len(state.tasks) == 0 {
						delete(b.contexts, runtime.ContextID)
					}
				}
			}
		}
		client = runtime.Session.Client
		runtime.Session.Client = nil
	}
	b.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
}

func (b *broker) cancel(taskID a2a.TaskID) (*taskRuntime, error) {
	runtime, err := b.task(taskID)
	if err != nil {
		return nil, err
	}
	runtime.cancelRequested.Store(true)
	return runtime, nil
}

func (b *broker) Close() error {
	b.mu.Lock()
	clients := make([]*codexClient, 0, len(b.tasks))
	for _, runtime := range b.tasks {
		if runtime.Session != nil && runtime.Session.Client != nil {
			clients = append(clients, runtime.Session.Client)
			runtime.Session.Client = nil
		}
	}
	b.tasks = make(map[a2a.TaskID]*taskRuntime)
	b.mu.Unlock()

	var errs []error
	for _, client := range clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
