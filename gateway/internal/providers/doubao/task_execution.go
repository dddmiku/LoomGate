package doubao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"workbuddy2api/internal/inference"
)

// TaskWorkRequest is internal to the activity adapter. It has no HTTP route.
// Task identity is re-read from the official catalog before any submission.
type TaskWorkRequest struct {
	AccountID, Revision, Model, Instruction, ActionKey string
	Task                                               Task
}

func (TaskWorkRequest) String() string   { return "doubao.TaskWorkRequest[redacted]" }
func (TaskWorkRequest) GoString() string { return "doubao.TaskWorkRequest[redacted]" }

type TaskWorkExecutor interface {
	CompleteTask(context.Context, TaskWorkRequest) (inference.Result, error)
}
type taskAction struct {
	Group    string `json:"group"`
	Code     string `json:"task"`
	Instance string `json:"instance"`
}

func (p *TextProvider) CompleteTask(ctx context.Context, in TaskWorkRequest) (inference.Result, error) {
	if in.ActionKey == "" || in.AccountID == "" || in.Revision == "" || in.Task.Group == "" || in.Task.Code == "" || in.Task.Work == nil || !in.Task.Work.Available {
		return inference.Result{}, runtimeError("desktop_task_manual")
	}
	owner := sha256.Sum256([]byte("task-work\x00" + in.AccountID))
	q := inference.Dialogue{Owner: hex.EncodeToString(owner[:]), Model: in.Model, AccountID: in.AccountID, AccountRevision: in.Revision, IdempotencyKey: in.ActionKey, Messages: []inference.Message{{Role: "user", Content: in.Instruction}}}
	return p.completeDialogue(ctx, q, nil, &taskAction{Group: in.Task.Group, Code: in.Task.Code, Instance: in.Task.ID})
}
