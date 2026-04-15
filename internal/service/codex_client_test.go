package service

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestLaunchCodexClientIncludesStderrOnInitializeFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultCwd = t.TempDir()
	cfg.CodexAppServerBin = os.Args[0]
	cfg.CodexCLI = ""
	cfg.CodexArgs = []string{"-test.run=TestFakeCodexHelperProcess", "--"}
	cfg.ChildEnv = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"FAKE_CODEX_FAIL_BEFORE_INITIALIZE=synthetic stderr failure",
	}

	_, err := launchCodexClient(context.Background(), cfg)
	if err == nil {
		t.Fatal("launchCodexClient() error = nil, want initialize failure")
	}
	if !strings.Contains(err.Error(), "initialize codex app-server") {
		t.Fatalf("launchCodexClient() error = %q, want initialize context", err)
	}
	if !strings.Contains(err.Error(), "synthetic stderr failure") {
		t.Fatalf("launchCodexClient() error = %q, want stderr details", err)
	}
}

func TestLaunchCodexClientCreatesConfiguredCodexHome(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultCwd = t.TempDir()
	cfg.CodexAppServerBin = os.Args[0]
	cfg.CodexCLI = ""
	cfg.CodexArgs = []string{"-test.run=TestFakeCodexHelperProcess", "--"}

	codexHome := t.TempDir() + "/missing/.codex"
	cfg.ChildEnv = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"FAKE_CODEX_REQUIRE_CODEX_HOME=1",
		"CODEX_HOME=" + codexHome,
	}

	client, err := launchCodexClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("launchCodexClient() error = %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	info, err := os.Stat(codexHome)
	if err != nil {
		t.Fatalf("Stat(CODEX_HOME) error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("CODEX_HOME %q is not a directory", codexHome)
	}
}
