package flowchart

import (
	"errors"
)

var (
	ErrWorkflowRunning          = errors.New("workflow is already running")
	ErrWorkflowResetRunning     = errors.New("cannot reset a running workflow")
	ErrWorkflowSubmitNodeToPool = errors.New("failed to submit node to pool")
	ErrWorkflowClosed           = errors.New("workflow has been closed and cannot be used")
)
