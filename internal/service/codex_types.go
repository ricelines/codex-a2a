package service

import "encoding/json"

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type initializeParams struct {
	ClientInfo struct {
		Name    string  `json:"name"`
		Title   *string `json:"title,omitempty"`
		Version string  `json:"version"`
	} `json:"clientInfo"`
	Capabilities *struct {
		ExperimentalAPI bool `json:"experimentalApi"`
	} `json:"capabilities,omitempty"`
}

type threadStartParams struct {
	Cwd                    string `json:"cwd,omitempty"`
	Model                  string `json:"model,omitempty"`
	ApprovalPolicy         string `json:"approvalPolicy,omitempty"`
	Sandbox                string `json:"sandbox,omitempty"`
	ExperimentalRawEvents  bool   `json:"experimentalRawEvents"`
	PersistExtendedHistory bool   `json:"persistExtendedHistory"`
}

type threadResumeParams struct {
	ThreadID               string `json:"threadId"`
	Cwd                    string `json:"cwd,omitempty"`
	Model                  string `json:"model,omitempty"`
	ApprovalPolicy         string `json:"approvalPolicy,omitempty"`
	Sandbox                string `json:"sandbox,omitempty"`
	PersistExtendedHistory bool   `json:"persistExtendedHistory"`
}

type threadForkParams struct {
	ThreadID               string `json:"threadId"`
	Cwd                    string `json:"cwd,omitempty"`
	Model                  string `json:"model,omitempty"`
	ApprovalPolicy         string `json:"approvalPolicy,omitempty"`
	Sandbox                string `json:"sandbox,omitempty"`
	PersistExtendedHistory bool   `json:"persistExtendedHistory"`
}

type threadResponse struct {
	Thread codexThread `json:"thread"`
}

type codexThread struct {
	ID string `json:"id"`
}

type turnStartParams struct {
	ThreadID       string           `json:"threadId"`
	Input          []codexUserInput `json:"input"`
	Cwd            string           `json:"cwd,omitempty"`
	Model          string           `json:"model,omitempty"`
	ApprovalPolicy string           `json:"approvalPolicy,omitempty"`
	SandboxPolicy  map[string]any   `json:"sandboxPolicy,omitempty"`
}

type turnStartResponse struct {
	Turn codexTurn `json:"turn"`
}

type codexTurn struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Error  *turnError   `json:"error,omitempty"`
	Items  []threadItem `json:"items,omitempty"`
}

type turnError struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo,omitempty"`
}

type codexUserInput struct {
	Type string `json:"type"`

	Text         string `json:"text,omitempty"`
	URL          string `json:"url,omitempty"`
	Path         string `json:"path,omitempty"`
	Name         string `json:"name,omitempty"`
	TextElements []any  `json:"textElements,omitempty"`
}

type threadItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	Text             string           `json:"text,omitempty"`
	Command          string           `json:"command,omitempty"`
	Cwd              string           `json:"cwd,omitempty"`
	Status           string           `json:"status,omitempty"`
	CommandActions   []map[string]any `json:"commandActions,omitempty"`
	AggregatedOutput *string          `json:"aggregatedOutput,omitempty"`
	ExitCode         *int             `json:"exitCode,omitempty"`
	Changes          []fileUpdate     `json:"changes,omitempty"`
	Server           string           `json:"server,omitempty"`
	Tool             string           `json:"tool,omitempty"`
	Arguments        any              `json:"arguments,omitempty"`
	Result           any              `json:"result,omitempty"`
	Error            *turnError       `json:"error,omitempty"`
	Query            string           `json:"query,omitempty"`
	Action           any              `json:"action,omitempty"`
	Summary          any              `json:"summary,omitempty"`
	Content          any              `json:"content,omitempty"`
	Phase            *string          `json:"phase,omitempty"`
}

type fileUpdate struct {
	Path string `json:"path"`
	Kind any    `json:"kind"`
	Diff string `json:"diff"`
}

type turnStartedNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     codexTurn `json:"turn"`
}

type turnCompletedNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     codexTurn `json:"turn"`
}

type turnDiffUpdatedNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Diff     string `json:"diff"`
}

type turnPlanUpdatedNotification struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	Explanation *string         `json:"explanation,omitempty"`
	Plan        []codexPlanStep `json:"plan"`
}

type codexPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type itemNotification struct {
	ThreadID string     `json:"threadId"`
	TurnID   string     `json:"turnId"`
	Item     threadItem `json:"item"`
}

type deltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type serverRequestResolvedNotification struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}

type commandApprovalRequest struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	ItemID   string          `json:"itemId"`
	Reason   *string         `json:"reason,omitempty"`
	Command  *string         `json:"command,omitempty"`
	Cwd      *string         `json:"cwd,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

type fileApprovalRequest struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	ItemID   string          `json:"itemId"`
	Reason   *string         `json:"reason,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

type elicitationRequest struct {
	ThreadID   string          `json:"threadId"`
	TurnID     *string         `json:"turnId,omitempty"`
	ServerName string          `json:"serverName"`
	Mode       string          `json:"mode"`
	Message    string          `json:"message"`
	URL        *string         `json:"url,omitempty"`
	Schema     json.RawMessage `json:"requestedSchema,omitempty"`
	Raw        json.RawMessage `json:"-"`
}
