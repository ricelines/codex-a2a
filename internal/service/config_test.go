package service

import (
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestRequestOptionsFromConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultCwd = "/default"
	cfg.DefaultModel = "gpt-default"

	got, err := requestOptionsFromConfig(cfg)
	if err != nil {
		t.Fatalf("requestOptionsFromConfig() error = %v", err)
	}
	if got.Cwd != "/default" {
		t.Fatalf("cwd = %q, want %q", got.Cwd, "/default")
	}
	if got.Model != "gpt-default" {
		t.Fatalf("model = %q, want %q", got.Model, "gpt-default")
	}
	if got.ApprovalPolicy != "on-request" {
		t.Fatalf("approvalPolicy = %q, want %q", got.ApprovalPolicy, "on-request")
	}
	if got.Sandbox != "read-only" {
		t.Fatalf("sandbox = %q, want %q", got.Sandbox, "read-only")
	}
}

func TestRequestOptionsFromConfigRejectsInvalidDefaults(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultApprovalPolicy = "definitely-not-valid"
	if _, err := requestOptionsFromConfig(cfg); err == nil {
		t.Fatal("requestOptionsFromConfig() unexpectedly accepted invalid approval policy")
	}

	cfg = DefaultConfig()
	cfg.DefaultSandbox = "definitely-not-valid"
	if _, err := requestOptionsFromConfig(cfg); err == nil {
		t.Fatal("requestOptionsFromConfig() unexpectedly accepted invalid sandbox")
	}
}

func TestResponseForPending_TextApproval(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "accept"})
	pending := &pendingRequest{Kind: pendingCommandApproval}

	got, err := responseForPending(msg, pending)
	if err != nil {
		t.Fatalf("responseForPending() error = %v", err)
	}
	resp, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("response type = %T, want map[string]any", got)
	}
	if resp["decision"] != "accept" {
		t.Fatalf("decision = %#v, want accept", resp["decision"])
	}
}

func TestSandboxPolicyFromString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "read-only", in: "read-only", want: "readOnly"},
		{name: "workspace-write", in: "workspace-write", want: "workspaceWrite"},
		{name: "danger-full-access", in: "danger-full-access", want: "dangerFullAccess"},
		{name: "legacy alias", in: "workspaceWrite", want: "workspaceWrite"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sandboxPolicyFromString(tt.in)
			if got["type"] != tt.want {
				t.Fatalf("sandboxPolicyFromString(%q) = %#v, want type %q", tt.in, got, tt.want)
			}
		})
	}
}
