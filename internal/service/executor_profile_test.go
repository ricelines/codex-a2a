package service

import (
	"testing"
	"time"
)

func TestArtifactMetaIncludesTaskAndItemTimestamps(t *testing.T) {
	taskStartedAt := time.Now().UTC().Add(-5 * time.Second).Round(0)
	itemStartedAt := taskStartedAt.Add(3 * time.Second)

	runtime := &taskRuntime{
		TurnID:        "turn-123",
		StartedAt:     taskStartedAt,
		Session:       &taskSession{ThreadID: "thread-123"},
		itemStartedAt: map[string]time.Time{},
	}
	runtime.noteItemStarted("item-123", itemStartedAt)

	meta := artifactMeta(runtime, "item-123", "mcpToolCall")

	if got := meta["threadId"]; got != "thread-123" {
		t.Fatalf("threadId = %#v, want %q", got, "thread-123")
	}
	if got := meta["turnId"]; got != "turn-123" {
		t.Fatalf("turnId = %#v, want %q", got, "turn-123")
	}
	if got := meta["taskStartedAt"]; got != taskStartedAt.Format(time.RFC3339Nano) {
		t.Fatalf("taskStartedAt = %#v, want %q", got, taskStartedAt.Format(time.RFC3339Nano))
	}
	if got := meta["itemStartedAt"]; got != itemStartedAt.Format(time.RFC3339Nano) {
		t.Fatalf("itemStartedAt = %#v, want %q", got, itemStartedAt.Format(time.RFC3339Nano))
	}
	if got := meta["itemId"]; got != "item-123" {
		t.Fatalf("itemId = %#v, want %q", got, "item-123")
	}
	if got := meta["itemType"]; got != "mcpToolCall" {
		t.Fatalf("itemType = %#v, want %q", got, "mcpToolCall")
	}

	emittedRaw, ok := meta["emittedAt"].(string)
	if !ok || emittedRaw == "" {
		t.Fatalf("emittedAt = %#v, want non-empty string", meta["emittedAt"])
	}
	emittedAt, err := time.Parse(time.RFC3339Nano, emittedRaw)
	if err != nil {
		t.Fatalf("parse emittedAt %q: %v", emittedRaw, err)
	}
	if emittedAt.Before(itemStartedAt) {
		t.Fatalf("emittedAt = %s, want >= itemStartedAt %s", emittedAt.Format(time.RFC3339Nano), itemStartedAt.Format(time.RFC3339Nano))
	}
}
