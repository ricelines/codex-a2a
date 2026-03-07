package service

import (
	"github.com/a2aproject/a2a-go/a2asrv"
)

func NewHandler(executor *Executor) a2asrv.RequestHandler {
	taskStore := NewTaskStore()
	return a2asrv.NewHandler(
		executor,
		a2asrv.WithTaskStore(taskStore),
		a2asrv.WithRequestContextInterceptor(&a2asrv.ReferencedTasksLoader{Store: taskStore}),
	)
}
