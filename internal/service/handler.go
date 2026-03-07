package service

import (
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

func NewHandler(executor *Executor) a2asrv.RequestHandler {
	taskStore := NewTaskStore()
	return a2asrv.NewHandler(
		executor,
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithTaskStore(taskStore),
		a2asrv.WithExecutorContextInterceptor(&a2asrv.ReferencedTasksLoader{Store: taskStore}),
	)
}
