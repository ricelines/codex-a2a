package service

import (
	"fmt"
	"os"
	"strings"
)

// metadataNamespace is used only for wrapper-emitted task metadata.
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

// RequestOptions are derived from server-owned defaults.
type RequestOptions struct {
	Cwd            string
	Model          string
	ApprovalPolicy string
	Sandbox        string
}

func DefaultConfig() Config {
	cwd, _ := os.Getwd()
	return Config{
		AgentName:             "Codex A2A",
		AgentDescription:      "A task-oriented A2A server backed by Codex app-server.",
		DefaultCwd:            cwd,
		DefaultApprovalPolicy: "on-request",
		DefaultSandbox:        "read-only",
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
	if err := validateApprovalPolicy(c.DefaultApprovalPolicy); err != nil {
		return err
	}
	if err := validateSandbox(c.DefaultSandbox); err != nil {
		return err
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

func requestOptionsFromConfig(cfg Config) (RequestOptions, error) {
	options := RequestOptions{
		Cwd:            cfg.DefaultCwd,
		Model:          cfg.DefaultModel,
		ApprovalPolicy: cfg.DefaultApprovalPolicy,
		Sandbox:        cfg.DefaultSandbox,
	}

	if options.Cwd == "" {
		return RequestOptions{}, fmt.Errorf("no cwd configured; set a server default")
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
