package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestExecutorFileApprovalRoundTrip(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	firstRun, err := collectEvents(h.handler.SendStreamingMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("NEEDS_FILE_APPROVAL")),
	}))
	if err != nil {
		t.Fatalf("first SendStreamingMessage() error = %v", err)
	}
	assertHasTaskState(t, firstRun, a2a.TaskStateInputRequired)

	taskID, contextID := taskIdentity(t, firstRun)
	reply := a2a.NewMessageForTask(
		a2a.MessageRoleUser,
		a2a.TaskInfo{TaskID: taskID, ContextID: contextID},
		a2a.NewDataPart(map[string]any{"decision": "accept"}),
	)
	secondRun, err := collectEvents(h.handler.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: reply}))
	if err != nil {
		t.Fatalf("second SendStreamingMessage() error = %v", err)
	}
	assertHasArtifactText(t, secondRun, "hello.txt")
	assertHasArtifactText(t, secondRun, "file approval handled")
	assertHasTaskState(t, secondRun, a2a.TaskStateCompleted)
}

func TestExecutorElicitationRoundTrip(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	firstRun, err := collectEvents(h.handler.SendStreamingMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("NEEDS_ELICITATION")),
	}))
	if err != nil {
		t.Fatalf("first SendStreamingMessage() error = %v", err)
	}
	assertHasTaskState(t, firstRun, a2a.TaskStateInputRequired)

	taskID, contextID := taskIdentity(t, firstRun)
	reply := a2a.NewMessageForTask(
		a2a.MessageRoleUser,
		a2a.TaskInfo{TaskID: taskID, ContextID: contextID},
		a2a.NewDataPart(map[string]any{
			"action": "accept",
			"content": map[string]any{
				"value": "ok",
			},
		}),
	)
	secondRun, err := collectEvents(h.handler.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: reply}))
	if err != nil {
		t.Fatalf("second SendStreamingMessage() error = %v", err)
	}
	assertHasArtifactText(t, secondRun, "elicitation accepted")
	assertHasTaskState(t, secondRun, a2a.TaskStateCompleted)
}

func TestExecutorLinkedContextsUseReferenceTaskBranch(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	root := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER root")))

	followUp := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("WHAT_DO_YOU_REMEMBER"))
	followUp.ContextID = root.ContextID
	followUp.ReferenceTasks = []a2a.TaskID{root.ID}
	child := mustSendTask(ctx, t, h.handler, followUp)

	assertTaskArtifactContains(t, child, "REMEMBER root")
	if childThreadID := taskMetaValue(child, "threadId"); childThreadID == taskMetaValue(root, "threadId") {
		t.Fatalf("child task reused parent thread id %v", childThreadID)
	}

	ops := fakeOperations(t, h.stateDir)
	var forks int
	for _, op := range ops {
		if op.Method == "thread/fork" {
			forks++
		}
	}
	if forks == 0 {
		t.Fatalf("expected at least one thread/fork operation, got %#v", ops)
	}
}

func TestExecutorParallelBranchesRetainSeparateState(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 15*time.Second)
	defer cancel()

	root := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER root")))

	childA := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER child-a"))
	childA.ContextID = root.ContextID
	childA.ReferenceTasks = []a2a.TaskID{root.ID}
	taskA := mustSendTask(ctx, t, h.handler, childA)

	childB := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER child-b"))
	childB.ContextID = root.ContextID
	childB.ReferenceTasks = []a2a.TaskID{root.ID}
	taskB := mustSendTask(ctx, t, h.handler, childB)

	checkA := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("WHAT_DO_YOU_REMEMBER"))
	checkA.ContextID = root.ContextID
	checkA.ReferenceTasks = []a2a.TaskID{taskA.ID}
	resultA := mustSendTask(ctx, t, h.handler, checkA)

	checkB := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("WHAT_DO_YOU_REMEMBER"))
	checkB.ContextID = root.ContextID
	checkB.ReferenceTasks = []a2a.TaskID{taskB.ID}
	resultB := mustSendTask(ctx, t, h.handler, checkB)

	assertTaskArtifactContains(t, resultA, "REMEMBER root")
	assertTaskArtifactContains(t, resultA, "REMEMBER child-a")
	assertTaskArtifactDoesNotContain(t, resultA, "REMEMBER child-b")

	assertTaskArtifactContains(t, resultB, "REMEMBER root")
	assertTaskArtifactContains(t, resultB, "REMEMBER child-b")
	assertTaskArtifactDoesNotContain(t, resultB, "REMEMBER child-a")
}

func TestExecutorRejectsTerminalTaskReuse(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	root := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")))
	reuse := a2a.NewMessageForTask(a2a.MessageRoleUser, root.TaskInfo(), a2a.NewTextPart("reuse"))

	if _, err := h.handler.SendMessage(ctx, &a2a.SendMessageRequest{Message: reuse}); err == nil {
		t.Fatal("SendMessage() unexpectedly succeeded when reusing a completed task")
	}
}

func TestExecutorRejectsAmbiguousBranchedContextWithoutReference(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	root := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER root")))

	childA := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER child-a"))
	childA.ContextID = root.ContextID
	childA.ReferenceTasks = []a2a.TaskID{root.ID}
	mustSendTask(ctx, t, h.handler, childA)

	childB := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER child-b"))
	childB.ContextID = root.ContextID
	childB.ReferenceTasks = []a2a.TaskID{root.ID}
	mustSendTask(ctx, t, h.handler, childB)

	ambiguous := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("WHAT_DO_YOU_REMEMBER"))
	ambiguous.ContextID = root.ContextID
	task := mustSendTask(ctx, t, h.handler, ambiguous)

	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("task.Status.State = %s, want %s", task.Status.State, a2a.TaskStateFailed)
	}
	if task.Status.Message == nil || !strings.Contains(messageText(task.Status.Message), "referenceTaskIds") {
		t.Fatalf("task.Status.Message = %#v, want referenceTaskIds guidance", task.Status.Message)
	}
}

func TestExecutorUnlinkedContextsDoNotShareState(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	_ = mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("REMEMBER root")))
	unlinked := mustSendTask(ctx, t, h.handler, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("WHAT_DO_YOU_REMEMBER")))

	assertTaskArtifactContains(t, unlinked, "(empty)")
	assertTaskArtifactDoesNotContain(t, unlinked, "REMEMBER root")
}

func TestExecutorCloseTearsDownActiveCodexProcesses(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(newAuthedContext(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	stream := h.handler.SendStreamingMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("WAIT_FOREVER")),
	})
	go func() {
		_, err := collectEvents(stream)
		done <- err
	}()

	waitFor(t, 5*time.Second, func() bool {
		for _, op := range fakeOperations(t, h.stateDir) {
			if op.Method == "turn/start" && op.Prompt == "WAIT_FOREVER" {
				return true
			}
		}
		return false
	})

	if err := h.executor.Close(); err != nil {
		t.Fatalf("executor.Close() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate after executor.Close()")
	}
}

func assertTaskArtifactDoesNotContain(t *testing.T, task *a2a.Task, forbidden string) {
	t.Helper()
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			if strings.Contains(part.Text(), forbidden) {
				t.Fatalf("task artifacts unexpectedly included %q: %#v", forbidden, task.Artifacts)
			}
		}
	}
}
