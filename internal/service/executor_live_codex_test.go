package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestLiveCodexSmoke(t *testing.T) {
	if os.Getenv("CODEX_A2A_RUN_LIVE") == "" {
		t.Skip("set CODEX_A2A_RUN_LIVE=1 to run live Codex smoke tests")
	}

	bin := findRealCodexBinary(t)
	authPath := liveAuthPath(t)

	codexHome := t.TempDir()
	if err := os.Symlink(authPath, filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatalf("Symlink(auth.json) error = %v", err)
	}
	config := `model = "gpt-5.1-codex-mini"
model_reasoning_effort = "medium"
approval_policy = "never"
sandbox_mode = "read-only"
enable_request_compression = false
`
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}

	cfg := DefaultConfig()
	cfg.CodexCLI = bin
	cfg.DefaultCwd = t.TempDir()
	cfg.ChildEnv = []string{"CODEX_HOME=" + codexHome}

	executor, err := NewExecutor(cfg)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	defer func() {
		if err := executor.Close(); err != nil {
			t.Fatalf("executor.Close() error = %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(newAuthedContext(), 90*time.Second)
	defer cancel()

	task := mustSendTask(ctx, t, NewHandler(executor), a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Reply with just OK."}))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task.Status.State = %s, want %s", task.Status.State, a2a.TaskStateCompleted)
	}
	assertTaskArtifactContains(t, task, "OK")
}

func liveAuthPath(t *testing.T) string {
	t.Helper()

	if path := os.Getenv("CODEX_A2A_LIVE_AUTH_JSON"); path != "" {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}
	path := filepath.Join(home, ".codex", "auth.json")
	if _, err := os.Lstat(path); err != nil {
		t.Skipf("live auth file not available at %s", path)
	}
	return path
}
