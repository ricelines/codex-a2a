package service

import (
	"context"

	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/taskstore"
)

// NewTaskStore returns an in-memory task store that treats unauthenticated callers as a
// stable anonymous user so tasks/list works in the default local deployment.
func NewTaskStore() taskstore.Store {
	return taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: func(ctx context.Context) (string, error) {
			if callCtx, ok := a2asrv.CallContextFrom(ctx); ok && callCtx.User != nil && callCtx.User.Name != "" {
				return callCtx.User.Name, nil
			}
			return "anonymous", nil
		},
	})
}
