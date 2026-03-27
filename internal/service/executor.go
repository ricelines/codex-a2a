package service

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
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

func (e *Executor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	options, err := requestOptionsFromConfig(e.cfg)
	if err != nil {
		return err
	}

	firstEventSent := false
	if reqCtx.StoredTask == nil {
		firstEventSent = true
		if err := queue.Write(ctx, a2a.NewSubmittedTask(reqCtx, reqCtx.Message)); err != nil {
			return err
		}
	}

	if err := e.execute(ctx, reqCtx, options, queue); err != nil {
		e.broker.finishTask(reqCtx.TaskID)
		if firstEventSent || reqCtx.StoredTask != nil {
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: err.Error()})
			return queue.Write(ctx, statusWithMeta(reqCtx, a2a.TaskStateFailed, msg, nil))
		}
		return err
	}
	return nil
}

func (e *Executor) execute(
	ctx context.Context,
	reqCtx *a2asrv.RequestContext,
	options RequestOptions,
	queue eventqueue.Queue,
) error {
	switch {
	case reqCtx.StoredTask == nil:
		input, err := codexInputsFromMessage(reqCtx.Message)
		if err != nil {
			return err
		}
		runtime, err := e.broker.startTask(ctx, reqCtx.TaskID, reqCtx.ContextID, options, reqCtx.Message.ReferenceTasks)
		if err != nil {
			return err
		}
		return e.startNewTurn(ctx, reqCtx, runtime, options, input, queue)
	case reqCtx.StoredTask.Status.State == a2a.TaskStateInputRequired:
		runtime, err := e.broker.task(reqCtx.TaskID)
		if err != nil {
			return err
		}
		if runtime.pendingRequest() != nil {
			return e.resumeInputRequired(ctx, reqCtx, runtime, queue)
		}
		input, err := codexInputsFromMessage(reqCtx.Message)
		if err != nil {
			return err
		}
		return e.startNewTurn(ctx, reqCtx, runtime, options, input, queue)
	default:
		return fmt.Errorf("task %s is in state %s; only INPUT_REQUIRED tasks may be continued", reqCtx.TaskID, reqCtx.StoredTask.Status.State)
	}
}

func (e *Executor) startNewTurn(
	ctx context.Context,
	reqCtx *a2asrv.RequestContext,
	runtime *taskRuntime,
	options RequestOptions,
	input []codexUserInput,
	queue eventqueue.Queue,
) error {
	respRaw, err := runtime.Session.Client.request(ctx, "turn/start", turnStartParams{
		ThreadID:       runtime.Session.ThreadID,
		Input:          input,
		Cwd:            options.Cwd,
		Model:          options.Model,
		ApprovalPolicy: options.ApprovalPolicy,
		SandboxPolicy:  sandboxPolicyFromString(options.Sandbox),
	})
	if err != nil {
		e.broker.discardTask(reqCtx.TaskID)
		return fmt.Errorf("codex turn/start: %w", err)
	}
	resp, err := decodeJSON[turnStartResponse](respRaw)
	if err != nil {
		e.broker.discardTask(reqCtx.TaskID)
		return fmt.Errorf("decode turn/start response: %w", err)
	}

	runtime.startTurn(resp.Turn.ID)
	if err := queue.Write(ctx, statusWithMeta(reqCtx, a2a.TaskStateWorking, nil, codexTaskMeta(runtime))); err != nil {
		return err
	}
	return e.consumeTurn(ctx, reqCtx, runtime, queue)
}

func (e *Executor) resumeInputRequired(
	ctx context.Context,
	reqCtx *a2asrv.RequestContext,
	runtime *taskRuntime,
	queue eventqueue.Queue,
) error {
	pending := runtime.pendingRequest()
	if pending == nil {
		e.broker.finishTask(reqCtx.TaskID)
		return fmt.Errorf("task %s is INPUT_REQUIRED but broker has no pending Codex request", reqCtx.TaskID)
	}

	response, err := responseForPending(reqCtx.Message, pending)
	if err != nil {
		if err := queue.Write(ctx, pendingArtifact(reqCtx, pending, err.Error())); err != nil {
			return err
		}
		return queue.Write(ctx, inputRequiredStatus(reqCtx, pending, err.Error()))
	}

	if err := runtime.Session.Client.respond(ctx, pending.ID, response); err != nil {
		e.broker.finishTask(reqCtx.TaskID)
		return fmt.Errorf("respond to pending Codex request: %w", err)
	}
	runtime.clearPending()
	if err := queue.Write(ctx, statusWithMeta(reqCtx, a2a.TaskStateWorking, nil, codexTaskMeta(runtime))); err != nil {
		return err
	}
	return e.consumeTurn(ctx, reqCtx, runtime, queue)
}

func (e *Executor) consumeTurn(
	ctx context.Context,
	reqCtx *a2asrv.RequestContext,
	runtime *taskRuntime,
	queue eventqueue.Queue,
) error {
	for {
		msg, err := runtime.Session.Client.next(ctx)
		if err != nil {
			e.broker.finishTask(reqCtx.TaskID)
			return err
		}

		if len(msg.ID) > 0 {
			pending, events, shouldPause, err := e.handleServerRequest(reqCtx, runtime, msg)
			if err != nil {
				e.broker.finishTask(reqCtx.TaskID)
				return err
			}
			if err := writeEvents(ctx, queue, events); err != nil {
				return err
			}
			if shouldPause {
				runtime.setPending(pending)
				return nil
			}
			continue
		}

		events, outcome, err := e.handleNotification(reqCtx, runtime, msg)
		if err != nil {
			e.broker.finishTask(reqCtx.TaskID)
			return err
		}
		if err := writeEvents(ctx, queue, events); err != nil {
			return err
		}
		switch outcome {
		case turnOutcomeContinue:
			continue
		case turnOutcomePause:
			return nil
		case turnOutcomeFinish:
			e.broker.finishTask(reqCtx.TaskID)
			return nil
		}
	}
}

type turnOutcome int

const (
	turnOutcomeContinue turnOutcome = iota
	turnOutcomePause
	turnOutcomeFinish
)

func (e *Executor) handleServerRequest(reqCtx *a2asrv.RequestContext, runtime *taskRuntime, msg incomingMessage) (*pendingRequest, []a2a.Event, bool, error) {
	switch msg.Method {
	case "item/commandExecution/requestApproval":
		req, err := decodeJSON[commandApprovalRequest](msg.Params)
		if err != nil {
			return nil, nil, false, err
		}
		req.Raw = msg.Params
		pending := &pendingRequest{ID: msg.ID, Kind: pendingCommandApproval, CommandApproval: &req}
		return pending, []a2a.Event{pendingArtifact(reqCtx, pending, ""), inputRequiredStatus(reqCtx, pending, "")}, true, nil
	case "item/fileChange/requestApproval":
		req, err := decodeJSON[fileApprovalRequest](msg.Params)
		if err != nil {
			return nil, nil, false, err
		}
		req.Raw = msg.Params
		pending := &pendingRequest{ID: msg.ID, Kind: pendingFileApproval, FileApproval: &req}
		return pending, []a2a.Event{pendingArtifact(reqCtx, pending, ""), inputRequiredStatus(reqCtx, pending, "")}, true, nil
	case "mcpServer/elicitation/request":
		req, err := decodeJSON[elicitationRequest](msg.Params)
		if err != nil {
			return nil, nil, false, err
		}
		req.Raw = msg.Params
		pending := &pendingRequest{ID: msg.ID, Kind: pendingElicitation, Elicitation: &req}
		return pending, []a2a.Event{pendingArtifact(reqCtx, pending, ""), inputRequiredStatus(reqCtx, pending, "")}, true, nil
	default:
		return nil, nil, false, fmt.Errorf("unsupported Codex server request %q", msg.Method)
	}
}

func (e *Executor) handleNotification(reqCtx *a2asrv.RequestContext, runtime *taskRuntime, msg incomingMessage) ([]a2a.Event, turnOutcome, error) {
	switch msg.Method {
	case "turn/started":
		return nil, turnOutcomeContinue, nil
	case "turn/completed":
		n, err := decodeJSON[turnCompletedNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		switch n.Turn.Status {
		case "completed":
			runtime.finishTurnForNextMessage()
			return []a2a.Event{readyForNextTurnStatus(reqCtx, runtime)}, turnOutcomePause, nil
		case "interrupted":
			if runtime.cancelRequested.Load() {
				return nil, turnOutcomeFinish, nil
			}
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: "Codex interrupted the turn."})
			return []a2a.Event{statusWithMeta(reqCtx, a2a.TaskStateCanceled, msg, codexTaskMeta(runtime))}, turnOutcomeFinish, nil
		case "failed":
			message := "Codex turn failed."
			if n.Turn.Error != nil && n.Turn.Error.Message != "" {
				message = n.Turn.Error.Message
			}
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: message})
			return []a2a.Event{statusWithMeta(reqCtx, a2a.TaskStateFailed, msg, codexTaskMeta(runtime))}, turnOutcomeFinish, nil
		default:
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: "Codex returned an unknown terminal status."})
			return []a2a.Event{statusWithMeta(reqCtx, a2a.TaskStateFailed, msg, codexTaskMeta(runtime))}, turnOutcomeFinish, nil
		}
	case "turn/diff/updated":
		n, err := decodeJSON[turnDiffUpdatedNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		event := replaceArtifact(reqCtx, "turn:diff", "Unified Diff", artifactMeta(runtime, "", "diff"), a2a.TextPart{Text: n.Diff})
		return []a2a.Event{event}, turnOutcomeContinue, nil
	case "turn/plan/updated":
		n, err := decodeJSON[turnPlanUpdatedNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		data := map[string]any{"plan": n.Plan}
		if n.Explanation != nil {
			data["explanation"] = *n.Explanation
		}
		event := replaceArtifact(reqCtx, "turn:plan", "Plan", artifactMeta(runtime, "", "plan"), a2a.DataPart{Data: data})
		return []a2a.Event{event}, turnOutcomeContinue, nil
	case "item/agentMessage/delta":
		n, err := decodeJSON[deltaNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		text := runtime.appendAssistantText(n.ItemID, n.Delta)
		event := replaceArtifact(reqCtx, artifactID("assistant", n.ItemID), "Assistant Response", artifactMeta(runtime, n.ItemID, "agentMessage"), a2a.TextPart{Text: text})
		return []a2a.Event{event}, turnOutcomeContinue, nil
	case "item/commandExecution/outputDelta":
		n, err := decodeJSON[deltaNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		runtime.appendCommandOutput(n.ItemID, n.Delta)
		event := appendArtifact(reqCtx, artifactID("command", n.ItemID), "Command Output", artifactMeta(runtime, n.ItemID, "commandExecution"), a2a.TextPart{Text: n.Delta})
		return []a2a.Event{event}, turnOutcomeContinue, nil
	case "item/fileChange/outputDelta":
		n, err := decodeJSON[deltaNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		runtime.appendFileChangeDiff(n.ItemID, n.Delta)
		event := appendArtifact(reqCtx, artifactID("file-change", n.ItemID), "File Change", artifactMeta(runtime, n.ItemID, "fileChange"), a2a.TextPart{Text: n.Delta})
		return []a2a.Event{event}, turnOutcomeContinue, nil
	case "item/started":
		n, err := decodeJSON[itemNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		runtime.noteItemStarted(n.Item.ID, time.Now().UTC())
		return startArtifactEvents(reqCtx, runtime, n.Item), turnOutcomeContinue, nil
	case "item/completed":
		n, err := decodeJSON[itemNotification](msg.Params)
		if err != nil {
			return nil, turnOutcomeContinue, err
		}
		return completedArtifactEvents(reqCtx, runtime, n.Item), turnOutcomeContinue, nil
	case "serverRequest/resolved":
		return nil, turnOutcomeContinue, nil
	default:
		return nil, turnOutcomeContinue, nil
	}
}

func codexInputsFromMessage(msg *a2a.Message) ([]codexUserInput, error) {
	inputs := make([]codexUserInput, 0, len(msg.Parts))
	var textParts []string
	for _, part := range msg.Parts {
		switch typed := part.(type) {
		case a2a.TextPart:
			if typed.Text != "" {
				textParts = append(textParts, typed.Text)
			}
		case *a2a.TextPart:
			if typed.Text != "" {
				textParts = append(textParts, typed.Text)
			}
		case a2a.DataPart:
			blob, err := json.MarshalIndent(typed.Data, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshal data part: %w", err)
			}
			textParts = append(textParts, string(blob))
		case *a2a.DataPart:
			blob, err := json.MarshalIndent(typed.Data, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshal data part: %w", err)
			}
			textParts = append(textParts, string(blob))
		case a2a.FilePart:
			input, err := codexInputFromFilePart(typed)
			if err != nil {
				return nil, err
			}
			inputs = append(inputs, input)
		case *a2a.FilePart:
			input, err := codexInputFromFilePart(*typed)
			if err != nil {
				return nil, err
			}
			inputs = append(inputs, input)
		default:
			return nil, fmt.Errorf("unsupported message part type %T", part)
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

func codexInputFromFilePart(part a2a.FilePart) (codexUserInput, error) {
	uri, ok := part.File.(a2a.FileURI)
	if !ok {
		return codexUserInput{}, fmt.Errorf("embedded file parts are not supported by this wrapper; provide a URI-backed file part instead")
	}
	if uri.MimeType != "" && !strings.HasPrefix(uri.MimeType, "image/") {
		return codexUserInput{}, fmt.Errorf("file parts are only supported for image MIME types, got %q", uri.MimeType)
	}
	return codexUserInput{Type: "image", URL: uri.URI}, nil
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
		switch typed := part.(type) {
		case a2a.DataPart:
			return typed.Data, true
		case *a2a.DataPart:
			return typed.Data, true
		}
	}
	return nil, false
}

func messageText(msg *a2a.Message) string {
	var parts []string
	for _, part := range msg.Parts {
		if text := partText(part); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func partText(part a2a.Part) string {
	switch typed := part.(type) {
	case a2a.TextPart:
		return typed.Text
	case *a2a.TextPart:
		return typed.Text
	default:
		return ""
	}
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
	event.Final = state.Terminal() || state == a2a.TaskStateAuthRequired || state == a2a.TaskStateInputRequired
	if meta != nil {
		event.SetMeta(metadataNamespace, meta)
	}
	return event
}

func appendArtifact(info a2a.TaskInfoProvider, id, name string, meta map[string]any, parts ...a2a.Part) *a2a.TaskArtifactUpdateEvent {
	event := a2a.NewArtifactUpdateEvent(info, a2a.ArtifactID(id), parts...)
	event.Artifact.Name = name
	if meta != nil {
		event.Artifact.Metadata = meta
	}
	return event
}

func replaceArtifact(info a2a.TaskInfoProvider, id, name string, meta map[string]any, parts ...a2a.Part) *a2a.TaskArtifactUpdateEvent {
	taskInfo := info.TaskInfo()
	return &a2a.TaskArtifactUpdateEvent{
		TaskID:    taskInfo.TaskID,
		ContextID: taskInfo.ContextID,
		Artifact: &a2a.Artifact{
			ID:       a2a.ArtifactID(id),
			Name:     name,
			Parts:    a2a.ContentParts(parts),
			Metadata: meta,
		},
	}
}

func artifactID(prefix, itemID string) string {
	return prefix + ":" + itemID
}

func codexTaskMeta(runtime *taskRuntime) map[string]any {
	return map[string]any{
		"threadId":      runtime.Session.ThreadID,
		"turnId":        runtime.TurnID,
		"taskStartedAt": runtime.StartedAt.UTC().Format(time.RFC3339Nano),
	}
}

func artifactMeta(runtime *taskRuntime, itemID, itemType string) map[string]any {
	meta := codexTaskMeta(runtime)
	meta["emittedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	if itemID != "" {
		meta["itemId"] = itemID
		if startedAt, ok := runtime.itemStartTime(itemID); ok {
			meta["itemStartedAt"] = startedAt.UTC().Format(time.RFC3339Nano)
		}
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
	return replaceArtifact(info, "pending:user-input", "Pending User Input", map[string]any{"kind": string(pending.Kind)}, a2a.DataPart{Data: data})
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
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.TextPart{Text: text})
	return statusWithMeta(info, a2a.TaskStateInputRequired, msg, map[string]any{"pendingKind": string(pending.Kind)})
}

func readyForNextTurnStatus(info a2a.TaskInfoProvider, runtime *taskRuntime) *a2a.TaskStatusUpdateEvent {
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.TextPart{Text: "Waiting for the next user message."})
	return statusWithMeta(info, a2a.TaskStateInputRequired, msg, codexTaskMeta(runtime))
}

func startArtifactEvents(info a2a.TaskInfoProvider, runtime *taskRuntime, item threadItem) []a2a.Event {
	switch item.Type {
	case "commandExecution":
		return []a2a.Event{
			replaceArtifact(info, artifactID("command", item.ID), "Command Output", artifactMeta(runtime, item.ID, item.Type), a2a.DataPart{Data: map[string]any{
				"command": item.Command,
				"cwd":     item.Cwd,
				"status":  item.Status,
				"actions": item.CommandActions,
			}}),
		}
	case "fileChange":
		return []a2a.Event{
			replaceArtifact(info, artifactID("file-change", item.ID), "File Change", artifactMeta(runtime, item.ID, item.Type), a2a.DataPart{Data: map[string]any{
				"changes": item.Changes,
				"status":  item.Status,
			}}),
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
			replaceArtifact(info, artifactID("assistant", item.ID), "Assistant Response", artifactMeta(runtime, item.ID, item.Type), a2a.TextPart{Text: item.Text}),
		}
	case "commandExecution":
		parts := []a2a.Part{
			a2a.DataPart{Data: map[string]any{
				"command":          item.Command,
				"cwd":              item.Cwd,
				"status":           item.Status,
				"actions":          item.CommandActions,
				"aggregatedOutput": firstNonEmpty(item.AggregatedOutput, runtime.commandOutput(item.ID)),
				"exitCode":         item.ExitCode,
			}},
		}
		if out := firstNonEmpty(item.AggregatedOutput, runtime.commandOutput(item.ID)); out != "" {
			parts = append(parts, a2a.TextPart{Text: out})
		}
		return []a2a.Event{
			replaceArtifact(info, artifactID("command", item.ID), "Command Output", artifactMeta(runtime, item.ID, item.Type), parts...),
		}
	case "fileChange":
		parts := []a2a.Part{
			a2a.DataPart{Data: map[string]any{
				"changes": item.Changes,
				"status":  item.Status,
			}},
		}
		if diff := runtime.fileChangeDiff(item.ID); diff != "" {
			parts = append(parts, a2a.TextPart{Text: diff})
		}
		return []a2a.Event{
			replaceArtifact(info, artifactID("file-change", item.ID), "File Change", artifactMeta(runtime, item.ID, item.Type), parts...),
		}
	case "mcpToolCall", "reasoning", "webSearch":
		return []a2a.Event{
			replaceArtifact(info, artifactID(item.Type, item.ID), item.Type, artifactMeta(runtime, item.ID, item.Type), a2a.DataPart{Data: structMap(item)}),
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

func structMap(v any) map[string]any {
	blob, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out := make(map[string]any)
	if err := json.Unmarshal(blob, &out); err != nil {
		panic(err)
	}
	return out
}

func writeEvents(ctx context.Context, queue eventqueue.Queue, events []a2a.Event) error {
	for _, event := range events {
		if err := queue.Write(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	runtime, err := e.broker.cancel(reqCtx.TaskID)
	if err != nil {
		return err
	}
	if runtime.TurnID != "" {
		_, _ = runtime.Session.Client.request(ctx, "turn/interrupt", map[string]any{
			"threadId": runtime.Session.ThreadID,
			"turnId":   runtime.TurnID,
		})
	}
	e.broker.finishTask(reqCtx.TaskID)
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: "Canceled by the user."})
	return queue.Write(ctx, statusWithMeta(reqCtx, a2a.TaskStateCanceled, msg, codexTaskMeta(runtime)))
}
