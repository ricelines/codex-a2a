package service

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

func init() {
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register([]map[string]any{})
	gob.Register([]string{})
	gob.Register(json.RawMessage{})
	gob.Register(pendingKind(""))
	gob.Register(codexPlanStep{})
	gob.Register([]codexPlanStep{})
	gob.Register(fileUpdate{})
	gob.Register([]fileUpdate{})
	gob.Register(threadItem{})
	gob.Register([]threadItem{})
}

// Executor adapts Codex app-server turns into A2A tasks.
type Executor struct {
	cfg    Config
	broker *broker
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

func NewExecutor(cfg Config) (*Executor, error) {
	b, err := newBroker(cfg)
	if err != nil {
		return nil, err
	}
	return &Executor{cfg: cfg, broker: b}, nil
}

func (e *Executor) Close() error {
	return e.broker.Close()
}

func (e *Executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		options, err := mergeRequestOptions(e.cfg, execCtx.Metadata, execCtx.Message.Metadata)
		if err != nil {
			yield(nil, err)
			return
		}

		firstEventSent := false
		if execCtx.StoredTask == nil {
			firstEventSent = true
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		if err := e.execute(ctx, execCtx, options, yield); err != nil {
			e.broker.finishTask(execCtx.TaskID)
			if firstEventSent || execCtx.StoredTask != nil {
				msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(err.Error()))
				yield(statusWithMeta(execCtx, a2a.TaskStateFailed, msg, nil), nil)
				return
			}
			yield(nil, err)
		}
	}
}

func (e *Executor) execute(
	ctx context.Context,
	execCtx *a2asrv.ExecutorContext,
	options RequestOptions,
	yield func(a2a.Event, error) bool,
) error {
	switch {
	case execCtx.StoredTask == nil:
		runtime, err := e.broker.startTask(ctx, execCtx.TaskID, execCtx.ContextID, options, execCtx.Message.ReferenceTasks)
		if err != nil {
			return err
		}
		return e.startNewTurn(ctx, execCtx, runtime, options, yield)
	case execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired:
		runtime, err := e.broker.task(execCtx.TaskID)
		if err != nil {
			return err
		}
		return e.resumeInputRequired(ctx, execCtx, runtime, yield)
	default:
		return fmt.Errorf("task %s is in state %s; only INPUT_REQUIRED tasks may be continued", execCtx.TaskID, execCtx.StoredTask.Status.State)
	}
}

func (e *Executor) startNewTurn(
	ctx context.Context,
	execCtx *a2asrv.ExecutorContext,
	runtime *taskRuntime,
	options RequestOptions,
	yield func(a2a.Event, error) bool,
) error {
	input, err := codexInputsFromMessage(execCtx.Message)
	if err != nil {
		e.broker.finishTask(execCtx.TaskID)
		return err
	}

	respRaw, err := runtime.Session.Client.request(ctx, "turn/start", turnStartParams{
		ThreadID:       runtime.Session.ThreadID,
		Input:          input,
		Cwd:            options.Cwd,
		Model:          options.Model,
		ApprovalPolicy: options.ApprovalPolicy,
		SandboxPolicy:  sandboxPolicyFromString(options.Sandbox),
	})
	if err != nil {
		e.broker.finishTask(execCtx.TaskID)
		return fmt.Errorf("codex turn/start: %w", err)
	}
	resp, err := decodeJSON[turnStartResponse](respRaw)
	if err != nil {
		e.broker.finishTask(execCtx.TaskID)
		return fmt.Errorf("decode turn/start response: %w", err)
	}

	runtime.TurnID = resp.Turn.ID
	if !yield(statusWithMeta(execCtx, a2a.TaskStateWorking, nil, codexTaskMeta(runtime)), nil) {
		return nil
	}
	return e.consumeTurn(ctx, execCtx, runtime, yield)
}

func (e *Executor) resumeInputRequired(
	ctx context.Context,
	execCtx *a2asrv.ExecutorContext,
	runtime *taskRuntime,
	yield func(a2a.Event, error) bool,
) error {
	pending := runtime.pendingRequest()
	if pending == nil {
		e.broker.finishTask(execCtx.TaskID)
		return fmt.Errorf("task %s is INPUT_REQUIRED but broker has no pending Codex request", execCtx.TaskID)
	}

	response, err := responseForPending(execCtx.Message, pending)
	if err != nil {
		if !yield(pendingArtifact(execCtx, pending, err.Error()), nil) {
			return nil
		}
		yield(inputRequiredStatus(execCtx, pending, err.Error()), nil)
		return nil
	}

	if err := runtime.Session.Client.respond(ctx, pending.ID, response); err != nil {
		e.broker.finishTask(execCtx.TaskID)
		return fmt.Errorf("respond to pending Codex request: %w", err)
	}
	runtime.clearPending()
	if !yield(statusWithMeta(execCtx, a2a.TaskStateWorking, nil, codexTaskMeta(runtime)), nil) {
		return nil
	}
	return e.consumeTurn(ctx, execCtx, runtime, yield)
}

func (e *Executor) consumeTurn(
	ctx context.Context,
	execCtx *a2asrv.ExecutorContext,
	runtime *taskRuntime,
	yield func(a2a.Event, error) bool,
) error {
	for {
		msg, err := runtime.Session.Client.next(ctx)
		if err != nil {
			e.broker.finishTask(execCtx.TaskID)
			return err
		}

		if len(msg.ID) > 0 {
			pending, events, shouldPause, err := e.handleServerRequest(execCtx, runtime, msg)
			if err != nil {
				e.broker.finishTask(execCtx.TaskID)
				return err
			}
			for _, event := range events {
				if !yield(event, nil) {
					return nil
				}
			}
			if shouldPause {
				runtime.setPending(pending)
				return nil
			}
			continue
		}

		events, terminal, err := e.handleNotification(execCtx, runtime, msg)
		if err != nil {
			e.broker.finishTask(execCtx.TaskID)
			return err
		}
		for _, event := range events {
			if !yield(event, nil) {
				return nil
			}
		}
		if terminal {
			e.broker.finishTask(execCtx.TaskID)
			return nil
		}
	}
}

func (e *Executor) handleServerRequest(execCtx *a2asrv.ExecutorContext, runtime *taskRuntime, msg incomingMessage) (*pendingRequest, []a2a.Event, bool, error) {
	switch msg.Method {
	case "item/commandExecution/requestApproval":
		req, err := decodeJSON[commandApprovalRequest](msg.Params)
		if err != nil {
			return nil, nil, false, err
		}
		req.Raw = msg.Params
		pending := &pendingRequest{ID: msg.ID, Kind: pendingCommandApproval, CommandApproval: &req}
		return pending, []a2a.Event{pendingArtifact(execCtx, pending, ""), inputRequiredStatus(execCtx, pending, "")}, true, nil
	case "item/fileChange/requestApproval":
		req, err := decodeJSON[fileApprovalRequest](msg.Params)
		if err != nil {
			return nil, nil, false, err
		}
		req.Raw = msg.Params
		pending := &pendingRequest{ID: msg.ID, Kind: pendingFileApproval, FileApproval: &req}
		return pending, []a2a.Event{pendingArtifact(execCtx, pending, ""), inputRequiredStatus(execCtx, pending, "")}, true, nil
	case "mcpServer/elicitation/request":
		req, err := decodeJSON[elicitationRequest](msg.Params)
		if err != nil {
			return nil, nil, false, err
		}
		req.Raw = msg.Params
		pending := &pendingRequest{ID: msg.ID, Kind: pendingElicitation, Elicitation: &req}
		return pending, []a2a.Event{pendingArtifact(execCtx, pending, ""), inputRequiredStatus(execCtx, pending, "")}, true, nil
	default:
		return nil, nil, false, fmt.Errorf("unsupported Codex server request %q", msg.Method)
	}
}

func (e *Executor) handleNotification(execCtx *a2asrv.ExecutorContext, runtime *taskRuntime, msg incomingMessage) ([]a2a.Event, bool, error) {
	switch msg.Method {
	case "turn/started":
		return nil, false, nil
	case "turn/completed":
		n, err := decodeJSON[turnCompletedNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		switch n.Turn.Status {
		case "completed":
			return []a2a.Event{statusWithMeta(execCtx, a2a.TaskStateCompleted, nil, codexTaskMeta(runtime))}, true, nil
		case "interrupted":
			if runtime.cancelRequested.Load() {
				return nil, true, nil
			}
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("Codex interrupted the turn."))
			return []a2a.Event{statusWithMeta(execCtx, a2a.TaskStateCanceled, msg, codexTaskMeta(runtime))}, true, nil
		case "failed":
			message := "Codex turn failed."
			if n.Turn.Error != nil && n.Turn.Error.Message != "" {
				message = n.Turn.Error.Message
			}
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(message))
			return []a2a.Event{statusWithMeta(execCtx, a2a.TaskStateFailed, msg, codexTaskMeta(runtime))}, true, nil
		default:
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("Codex returned an unknown terminal status."))
			return []a2a.Event{statusWithMeta(execCtx, a2a.TaskStateFailed, msg, codexTaskMeta(runtime))}, true, nil
		}
	case "turn/diff/updated":
		n, err := decodeJSON[turnDiffUpdatedNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		event := replaceArtifact(execCtx, "turn:diff", "Unified Diff", artifactMeta(runtime, "", "diff"), a2a.NewTextPart(n.Diff))
		return []a2a.Event{event}, false, nil
	case "turn/plan/updated":
		n, err := decodeJSON[turnPlanUpdatedNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		data := map[string]any{"plan": n.Plan}
		if n.Explanation != nil {
			data["explanation"] = *n.Explanation
		}
		event := replaceArtifact(execCtx, "turn:plan", "Plan", artifactMeta(runtime, "", "plan"), a2a.NewDataPart(data))
		return []a2a.Event{event}, false, nil
	case "item/agentMessage/delta":
		n, err := decodeJSON[deltaNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		text := runtime.appendAssistantText(n.ItemID, n.Delta)
		event := replaceArtifact(execCtx, artifactID("assistant", n.ItemID), "Assistant Response", artifactMeta(runtime, n.ItemID, "agentMessage"), a2a.NewTextPart(text))
		return []a2a.Event{event}, false, nil
	case "item/commandExecution/outputDelta":
		n, err := decodeJSON[deltaNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		runtime.appendCommandOutput(n.ItemID, n.Delta)
		event := appendArtifact(execCtx, artifactID("command", n.ItemID), "Command Output", artifactMeta(runtime, n.ItemID, "commandExecution"), a2a.NewTextPart(n.Delta))
		return []a2a.Event{event}, false, nil
	case "item/fileChange/outputDelta":
		n, err := decodeJSON[deltaNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		runtime.appendFileChangeDiff(n.ItemID, n.Delta)
		event := appendArtifact(execCtx, artifactID("file-change", n.ItemID), "File Change", artifactMeta(runtime, n.ItemID, "fileChange"), a2a.NewTextPart(n.Delta))
		return []a2a.Event{event}, false, nil
	case "item/started":
		n, err := decodeJSON[itemNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		return startArtifactEvents(execCtx, runtime, n.Item), false, nil
	case "item/completed":
		n, err := decodeJSON[itemNotification](msg.Params)
		if err != nil {
			return nil, false, err
		}
		return completedArtifactEvents(execCtx, runtime, n.Item), false, nil
	case "serverRequest/resolved":
		return nil, false, nil
	default:
		return nil, false, nil
	}
}

func codexInputsFromMessage(msg *a2a.Message) ([]codexUserInput, error) {
	inputs := make([]codexUserInput, 0, len(msg.Parts))
	var textParts []string
	for _, part := range msg.Parts {
		switch {
		case part.Text() != "":
			textParts = append(textParts, part.Text())
		case part.URL() != "":
			inputs = append(inputs, codexUserInput{Type: "image", URL: string(part.URL())})
		case part.Data() != nil:
			blob, err := json.MarshalIndent(part.Data(), "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshal data part: %w", err)
			}
			textParts = append(textParts, string(blob))
		case len(part.Raw()) > 0:
			return nil, fmt.Errorf("raw binary message parts are not supported by this wrapper")
		}
	}
	if len(textParts) > 0 {
		inputs = append([]codexUserInput{{
			Type: "text",
			Text: strings.Join(textParts, "\n\n"),
		}}, inputs...)
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("message does not contain any Codex-compatible content parts")
	}
	return inputs, nil
}

func responseForPending(msg *a2a.Message, pending *pendingRequest) (any, error) {
	if payload, ok := firstDataPart(msg); ok {
		switch pending.Kind {
		case pendingCommandApproval, pendingFileApproval:
			if decision, ok := payload["decision"]; ok {
				return map[string]any{"decision": decision}, nil
			}
			return nil, fmt.Errorf("approval replies need a data part with a decision field")
		case pendingElicitation:
			action, ok := payload["action"]
			if !ok {
				return nil, fmt.Errorf("elicitation replies need a data part with an action field")
			}
			return map[string]any{
				"action":  action,
				"content": payload["content"],
			}, nil
		}
	}

	text := strings.TrimSpace(messageText(msg))
	if text == "" {
		return nil, fmt.Errorf("reply must include either a structured data part or a text decision")
	}
	switch pending.Kind {
	case pendingCommandApproval, pendingFileApproval:
		switch strings.ToLower(text) {
		case "accept", "approve", "approved", "yes", "y":
			return map[string]any{"decision": "accept"}, nil
		case "decline", "deny", "reject", "no", "n":
			return map[string]any{"decision": "decline"}, nil
		case "cancel":
			return map[string]any{"decision": "cancel"}, nil
		default:
			return nil, fmt.Errorf("unrecognized approval response %q; use accept, decline, or cancel", text)
		}
	case pendingElicitation:
		switch strings.ToLower(text) {
		case "decline":
			return map[string]any{"action": "decline", "content": nil}, nil
		case "cancel":
			return map[string]any{"action": "cancel", "content": nil}, nil
		default:
			return nil, fmt.Errorf("elicitation replies should use a data part with action/content, or the text decline/cancel")
		}
	default:
		return nil, fmt.Errorf("unsupported pending request kind %s", pending.Kind)
	}
}

func firstDataPart(msg *a2a.Message) (map[string]any, bool) {
	for _, part := range msg.Parts {
		if data, ok := part.Data().(map[string]any); ok {
			return data, true
		}
	}
	return nil, false
}

func messageText(msg *a2a.Message) string {
	var parts []string
	for _, part := range msg.Parts {
		if text := part.Text(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func sandboxPolicyFromString(mode string) map[string]any {
	if mode == "" {
		return nil
	}
	switch mode {
	case "read-only", "readOnly":
		return map[string]any{"type": "readOnly"}
	case "workspace-write", "workspaceWrite":
		return map[string]any{"type": "workspaceWrite"}
	case "danger-full-access", "dangerFullAccess":
		return map[string]any{"type": "dangerFullAccess"}
	default:
		return map[string]any{"type": mode}
	}
}

func statusWithMeta(info a2a.TaskInfoProvider, state a2a.TaskState, msg *a2a.Message, meta map[string]any) *a2a.TaskStatusUpdateEvent {
	event := a2a.NewStatusUpdateEvent(info, state, msg)
	if meta != nil {
		event.SetMeta(metadataNamespace, meta)
	}
	return event
}

func appendArtifact(info a2a.TaskInfoProvider, id, name string, meta map[string]any, parts ...*a2a.Part) *a2a.TaskArtifactUpdateEvent {
	event := a2a.NewArtifactUpdateEvent(info, a2a.ArtifactID(id), parts...)
	event.Artifact.Name = name
	if meta != nil {
		event.Artifact.Metadata = meta
	}
	return event
}

func replaceArtifact(info a2a.TaskInfoProvider, id, name string, meta map[string]any, parts ...*a2a.Part) *a2a.TaskArtifactUpdateEvent {
	taskInfo := info.TaskInfo()
	return &a2a.TaskArtifactUpdateEvent{
		TaskID:    taskInfo.TaskID,
		ContextID: taskInfo.ContextID,
		Artifact: &a2a.Artifact{
			ID:       a2a.ArtifactID(id),
			Name:     name,
			Parts:    parts,
			Metadata: meta,
		},
	}
}

func artifactID(prefix, itemID string) string {
	return prefix + ":" + itemID
}

func codexTaskMeta(runtime *taskRuntime) map[string]any {
	return map[string]any{
		"threadId": runtime.Session.ThreadID,
		"turnId":   runtime.TurnID,
	}
}

func artifactMeta(runtime *taskRuntime, itemID, itemType string) map[string]any {
	meta := codexTaskMeta(runtime)
	if itemID != "" {
		meta["itemId"] = itemID
	}
	if itemType != "" {
		meta["itemType"] = itemType
	}
	return meta
}

func pendingArtifact(info a2a.TaskInfoProvider, pending *pendingRequest, note string) *a2a.TaskArtifactUpdateEvent {
	data := map[string]any{
		"kind": string(pending.Kind),
	}
	switch pending.Kind {
	case pendingCommandApproval:
		data["request"] = json.RawMessage(pending.CommandApproval.Raw)
	case pendingFileApproval:
		data["request"] = json.RawMessage(pending.FileApproval.Raw)
	case pendingElicitation:
		data["request"] = json.RawMessage(pending.Elicitation.Raw)
	}
	if note != "" {
		data["note"] = note
	}
	return replaceArtifact(info, "pending:user-input", "Pending User Input", map[string]any{"kind": string(pending.Kind)}, a2a.NewDataPart(data))
}

func inputRequiredStatus(info a2a.TaskInfoProvider, pending *pendingRequest, note string) *a2a.TaskStatusUpdateEvent {
	text := "Codex is waiting for user input."
	switch pending.Kind {
	case pendingCommandApproval:
		text = "Codex is waiting for command approval. Reply with text `accept`, `decline`, or `cancel`, or send a data part like {\"decision\":\"accept\"}."
	case pendingFileApproval:
		text = "Codex is waiting for file change approval. Reply with text `accept` or `decline`, or send a data part like {\"decision\":\"accept\"}."
	case pendingElicitation:
		text = "Codex is waiting for structured MCP input. Reply with a data part like {\"action\":\"accept\",\"content\":{...}}, or text `decline` / `cancel`."
	}
	if note != "" {
		text += " " + note
	}
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
	return statusWithMeta(info, a2a.TaskStateInputRequired, msg, map[string]any{"pendingKind": string(pending.Kind)})
}

func startArtifactEvents(info a2a.TaskInfoProvider, runtime *taskRuntime, item threadItem) []a2a.Event {
	switch item.Type {
	case "commandExecution":
		return []a2a.Event{
			replaceArtifact(info, artifactID("command", item.ID), "Command Output", artifactMeta(runtime, item.ID, item.Type), a2a.NewDataPart(map[string]any{
				"command": item.Command,
				"cwd":     item.Cwd,
				"status":  item.Status,
				"actions": item.CommandActions,
			})),
		}
	case "fileChange":
		return []a2a.Event{
			replaceArtifact(info, artifactID("file-change", item.ID), "File Change", artifactMeta(runtime, item.ID, item.Type), a2a.NewDataPart(map[string]any{
				"changes": item.Changes,
				"status":  item.Status,
			})),
		}
	default:
		return nil
	}
}

func completedArtifactEvents(info a2a.TaskInfoProvider, runtime *taskRuntime, item threadItem) []a2a.Event {
	switch item.Type {
	case "agentMessage":
		if item.Text == "" {
			return nil
		}
		return []a2a.Event{
			replaceArtifact(info, artifactID("assistant", item.ID), "Assistant Response", artifactMeta(runtime, item.ID, item.Type), a2a.NewTextPart(item.Text)),
		}
	case "commandExecution":
		parts := []*a2a.Part{
			a2a.NewDataPart(map[string]any{
				"command":          item.Command,
				"cwd":              item.Cwd,
				"status":           item.Status,
				"actions":          item.CommandActions,
				"aggregatedOutput": firstNonEmpty(item.AggregatedOutput, runtime.commandOutput(item.ID)),
				"exitCode":         item.ExitCode,
			}),
		}
		if out := firstNonEmpty(item.AggregatedOutput, runtime.commandOutput(item.ID)); out != "" {
			parts = append(parts, a2a.NewTextPart(out))
		}
		return []a2a.Event{
			replaceArtifact(info, artifactID("command", item.ID), "Command Output", artifactMeta(runtime, item.ID, item.Type), parts...),
		}
	case "fileChange":
		parts := []*a2a.Part{
			a2a.NewDataPart(map[string]any{
				"changes": item.Changes,
				"status":  item.Status,
			}),
		}
		if diff := runtime.fileChangeDiff(item.ID); diff != "" {
			parts = append(parts, a2a.NewTextPart(diff))
		}
		return []a2a.Event{
			replaceArtifact(info, artifactID("file-change", item.ID), "File Change", artifactMeta(runtime, item.ID, item.Type), parts...),
		}
	case "mcpToolCall", "reasoning", "webSearch":
		return []a2a.Event{
			replaceArtifact(info, artifactID(item.Type, item.ID), item.Type, artifactMeta(runtime, item.ID, item.Type), a2a.NewDataPart(item)),
		}
	default:
		return nil
	}
}

func firstNonEmpty(ptr *string, fallback string) string {
	if ptr != nil && *ptr != "" {
		return *ptr
	}
	return fallback
}

func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		runtime, err := e.broker.cancel(execCtx.TaskID)
		if err != nil {
			yield(nil, err)
			return
		}
		if runtime.TurnID != "" {
			_, _ = runtime.Session.Client.request(ctx, "turn/interrupt", map[string]any{
				"threadId": runtime.Session.ThreadID,
				"turnId":   runtime.TurnID,
			})
		}
		e.broker.finishTask(execCtx.TaskID)
		msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("Canceled by the user."))
		yield(statusWithMeta(execCtx, a2a.TaskStateCanceled, msg, codexTaskMeta(runtime)), nil)
	}
}
