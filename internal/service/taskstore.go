package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

type storedTask struct {
	task        *a2a.Task
	user        string
	version     a2a.TaskVersion
	lastUpdated time.Time
}

type taskStore struct {
	mu    sync.RWMutex
	tasks map[a2a.TaskID]*storedTask
}

// NewTaskStore returns an in-memory task store with per-user ownership checks.
// Unauthenticated callers still share a local anonymous view because this repo
// does not provide an auth layer of its own.
func NewTaskStore() a2asrv.TaskStore {
	return &taskStore{tasks: make(map[a2a.TaskID]*storedTask)}
}

func (s *taskStore) Save(ctx context.Context, task *a2a.Task, _ a2a.Event, prev a2a.TaskVersion) (a2a.TaskVersion, error) {
	copyTask, err := cloneTask(task)
	if err != nil {
		return a2a.TaskVersionMissing, err
	}

	user := taskStoreUser(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	version := a2a.TaskVersion(1)
	if stored := s.tasks[task.ID]; stored != nil {
		if stored.user != user {
			return a2a.TaskVersionMissing, a2a.ErrTaskNotFound
		}
		if prev != a2a.TaskVersionMissing && stored.version != prev {
			return a2a.TaskVersionMissing, a2a.ErrConcurrentTaskModification
		}
		version = stored.version + 1
	}

	s.tasks[task.ID] = &storedTask{
		task:        copyTask,
		user:        user,
		version:     version,
		lastUpdated: time.Now(),
	}
	return version, nil
}

func (s *taskStore) Get(ctx context.Context, taskID a2a.TaskID) (*a2a.Task, a2a.TaskVersion, error) {
	s.mu.RLock()
	stored := s.tasks[taskID]
	s.mu.RUnlock()

	if stored == nil || stored.user != taskStoreUser(ctx) {
		return nil, a2a.TaskVersionMissing, a2a.ErrTaskNotFound
	}

	task, err := cloneTask(stored.task)
	if err != nil {
		return nil, a2a.TaskVersionMissing, err
	}
	return task, stored.version, nil
}

func (s *taskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	pageSize := req.PageSize
	switch {
	case pageSize == 0:
		pageSize = 50
	case pageSize < 1 || pageSize > 100:
		return nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d", pageSize)
	}
	if req.HistoryLength < 0 {
		return nil, fmt.Errorf("history length must be non-negative integer, got %d", req.HistoryLength)
	}

	user := taskStoreUser(ctx)

	s.mu.RLock()
	filtered := make([]*storedTask, 0, len(s.tasks))
	for _, stored := range s.tasks {
		if stored.user != user {
			continue
		}
		if req.ContextID != "" && stored.task.ContextID != req.ContextID {
			continue
		}
		if req.Status != a2a.TaskStateUnspecified && stored.task.Status.State != req.Status {
			continue
		}
		if req.LastUpdatedAfter != nil && stored.lastUpdated.Before(*req.LastUpdatedAfter) {
			continue
		}
		filtered = append(filtered, stored)
	}
	s.mu.RUnlock()

	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].lastUpdated.Equal(filtered[j].lastUpdated) {
			return filtered[i].lastUpdated.After(filtered[j].lastUpdated)
		}
		return strings.Compare(string(filtered[i].task.ID), string(filtered[j].task.ID)) > 0
	})

	page, nextPageToken, err := paginateTasks(filtered, pageSize, req.PageToken)
	if err != nil {
		return nil, err
	}

	out := make([]*a2a.Task, 0, len(page))
	for _, stored := range page {
		task, err := cloneTask(stored.task)
		if err != nil {
			return nil, err
		}
		if req.HistoryLength > 0 && len(task.History) > req.HistoryLength {
			task.History = task.History[len(task.History)-req.HistoryLength:]
		}
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		out = append(out, task)
	}

	return &a2a.ListTasksResponse{
		Tasks:         out,
		TotalSize:     len(filtered),
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

func cloneTask(task *a2a.Task) (*a2a.Task, error) {
	blob, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("marshal task copy: %w", err)
	}
	var copy a2a.Task
	if err := json.Unmarshal(blob, &copy); err != nil {
		return nil, fmt.Errorf("unmarshal task copy: %w", err)
	}
	return &copy, nil
}

func taskStoreUser(ctx context.Context) string {
	if callCtx, ok := a2asrv.CallContextFrom(ctx); ok && callCtx.User != nil && callCtx.User.Name() != "" {
		return callCtx.User.Name()
	}
	return "anonymous"
}

func paginateTasks(tasks []*storedTask, pageSize int, pageToken string) ([]*storedTask, string, error) {
	start := 0
	if pageToken != "" {
		cursorTime, cursorTaskID, err := decodePageToken(pageToken)
		if err != nil {
			return nil, "", err
		}
		start = sort.Search(len(tasks), func(i int) bool {
			task := tasks[i]
			switch cmp := task.lastUpdated.Compare(cursorTime); {
			case cmp < 0:
				return true
			case cmp > 0:
				return false
			default:
				return strings.Compare(string(task.task.ID), string(cursorTaskID)) < 0
			}
		})
	}

	page := tasks[start:]
	if len(page) <= pageSize {
		return page, "", nil
	}

	last := page[pageSize-1]
	return page[:pageSize], encodePageToken(last.lastUpdated, last.task.ID), nil
}

func encodePageToken(updatedAt time.Time, taskID a2a.TaskID) string {
	return base64.URLEncoding.EncodeToString([]byte(updatedAt.Format(time.RFC3339Nano) + "_" + string(taskID)))
}

func decodePageToken(token string) (time.Time, a2a.TaskID, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", err
	}

	parts := strings.Split(string(decoded), "_")
	if len(parts) != 2 {
		return time.Time{}, "", a2a.ErrParseError
	}

	when, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	return when, a2a.TaskID(parts[1]), nil
}
