package service

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
)

const metadataNamespace = "codexA2A"

// Config controls the wrapper server and child Codex sessions.
type Config struct {
	AgentName        string
	AgentDescription string
	BaseURL          string

	DefaultCwd            string
	DefaultModel          string
	DefaultApprovalPolicy string
	DefaultSandbox        string

	CodexCLI          string
	CodexAppServerBin string
	CodexArgs         []string
	CodexClientName   string
	CodexClientTitle  string
	CodexClientVer    string
	ChildEnv          []string
}

// RequestOptions are accepted through A2A request/message metadata under codexA2A.
type RequestOptions struct {
	Cwd            string         `json:"cwd,omitempty"`
	Model          string         `json:"model,omitempty"`
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
	Sandbox        string         `json:"sandbox,omitempty"`
	ServiceName    string         `json:"serviceName,omitempty"`
	Config         map[string]any `json:"config,omitempty"`
}

func DefaultConfig() Config {
	cwd, _ := os.Getwd()
	return Config{
		AgentName:             "Codex A2A",
		AgentDescription:      "A task-oriented A2A server backed by Codex app-server.",
		DefaultCwd:            cwd,
		DefaultApprovalPolicy: "never",
		DefaultSandbox:        "workspace-write",
		CodexCLI:              "codex",
		CodexClientName:       "codex_a2a",
		CodexClientTitle:      "Codex A2A Wrapper",
		CodexClientVer:        "0.1.0",
	}
}

func (c Config) validate() error {
	if c.DefaultCwd == "" {
		return fmt.Errorf("default cwd is required")
	}
	if c.CodexClientName == "" {
		return fmt.Errorf("codex client name is required")
	}
	if c.CodexClientVer == "" {
		return fmt.Errorf("codex client version is required")
	}
	if c.CodexAppServerBin == "" && c.CodexCLI == "" {
		return fmt.Errorf("either codex app-server bin or codex cli must be configured")
	}
	return nil
}

func mergeRequestOptions(cfg Config, requestMeta map[string]any, messageMeta map[string]any) (RequestOptions, error) {
	options := RequestOptions{
		Cwd:            cfg.DefaultCwd,
		Model:          cfg.DefaultModel,
		ApprovalPolicy: cfg.DefaultApprovalPolicy,
		Sandbox:        cfg.DefaultSandbox,
	}

	for _, meta := range []map[string]any{messageMeta, requestMeta} {
		if meta == nil {
			continue
		}
		raw, ok := meta[metadataNamespace]
		if !ok {
			continue
		}
		parsed, err := decodeRequestOptions(raw)
		if err != nil {
			return RequestOptions{}, err
		}
		if parsed.Cwd != "" {
			options.Cwd = parsed.Cwd
		}
		if parsed.Model != "" {
			options.Model = parsed.Model
		}
		if parsed.ApprovalPolicy != "" {
			options.ApprovalPolicy = parsed.ApprovalPolicy
		}
		if parsed.Sandbox != "" {
			options.Sandbox = parsed.Sandbox
		}
		if parsed.ServiceName != "" {
			options.ServiceName = parsed.ServiceName
		}
		if parsed.Config != nil {
			if options.Config == nil {
				options.Config = make(map[string]any, len(parsed.Config))
			}
			maps.Copy(options.Config, parsed.Config)
		}
	}

	if options.Cwd == "" {
		return RequestOptions{}, fmt.Errorf("no cwd configured; set a server default or codexA2A.cwd metadata")
	}
	if err := validateApprovalPolicy(options.ApprovalPolicy); err != nil {
		return RequestOptions{}, err
	}
	sandbox, err := normalizeSandbox(options.Sandbox)
	if err != nil {
		return RequestOptions{}, err
	}
	options.Sandbox = sandbox
	return options, nil
}

func decodeRequestOptions(raw any) (RequestOptions, error) {
	if raw == nil {
		return RequestOptions{}, nil
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return RequestOptions{}, fmt.Errorf("marshal %s metadata: %w", metadataNamespace, err)
	}
	var options RequestOptions
	if err := json.Unmarshal(blob, &options); err != nil {
		return RequestOptions{}, fmt.Errorf("decode %s metadata: %w", metadataNamespace, err)
	}
	return options, nil
}

func validateApprovalPolicy(policy string) error {
	if policy == "" {
		return nil
	}
	switch policy {
	case "untrusted", "on-failure", "on-request", "never":
		return nil
	default:
		return fmt.Errorf("unsupported approval policy %q", policy)
	}
}

func validateSandbox(mode string) error {
	_, err := normalizeSandbox(mode)
	return err
}

func normalizeSandbox(mode string) (string, error) {
	if mode == "" {
		return "", nil
	}
	switch mode {
	case "read-only", "readOnly":
		return "read-only", nil
	case "workspace-write", "workspaceWrite":
		return "workspace-write", nil
	case "danger-full-access", "dangerFullAccess":
		return "danger-full-access", nil
	default:
		return "", fmt.Errorf("unsupported sandbox %q", mode)
	}
}

func splitEnv(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}
