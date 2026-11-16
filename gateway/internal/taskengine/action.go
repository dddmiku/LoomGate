package taskengine

import (
	"crypto/sha256"
	"fmt"
)

type actionEvidence struct {
	disposition, binding string
	stage                Stage
	code                 int
}
type actionReceipt struct {
	Attempt string `json:"attempt"`
	Stage   Stage  `json:"stage"`
	Binding string `json:"binding"`
}

func actionBinding(t Task, stage Stage) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(t.ProviderID+"\x00"+t.AccountID+"\x00"+t.Key+"\x00"+t.PeriodKey+"\x00"+string(stage)+"\x00"+t.actionAttempt)))
}

// ActionReference identifies only the engine-issued attempt for this stage.
// It is not acceptance evidence. Discovery cannot supply it and it is omitted
// from public JSON. Adapters can bind private recovery journals to this value.
func ActionReference(t Task, stage Stage) string {
	if len(t.actionAttempt) != 32 || t.actionStage != stage {
		return ""
	}
	return actionBinding(t, stage)
}

// NotSubmitted is permitted only before calling a mutating transport, or after
// explicit provider evidence that no operation was accepted. It releases only
// the provisional marker for this exact attempt, never a previous unknown.
func NotSubmitted(t Task, stage Stage, o Observation) Observation {
	o.action = &actionEvidence{disposition: "not_submitted", binding: actionBinding(t, stage), stage: stage}
	return o
}

// Accepted requires an actual validated business acceptance response. A nil
// error, HTTP200 or a current task phase alone is never evidence of acceptance.
func Accepted(t Task, stage Stage, o Observation) Observation {
	o.action = &actionEvidence{disposition: "accepted", binding: actionBinding(t, stage), stage: stage}
	return o
}
func UnknownAction(t Task, stage Stage, o Observation) Observation {
	o.action = &actionEvidence{disposition: "unknown", binding: actionBinding(t, stage), stage: stage}
	return o
}

// Rejected is for a documented, explicit no-effect business refusal, not a
// generic HTTP failure. Its safe numeric business code is retained in audit.
func Rejected(t Task, stage Stage, code int, o Observation) Observation {
	o.action = &actionEvidence{disposition: "rejected", binding: actionBinding(t, stage), stage: stage, code: code}
	return o
}
func actionOutcome(t Task, stage Stage, o Observation) string {
	if t.actionAttempt == "" || o.action == nil || o.action.stage != stage || o.action.binding != actionBinding(t, stage) {
		return "unknown"
	}
	return o.action.disposition
}
func receiptStage(t Task, r actionReceipt) Stage {
	if len(r.Attempt) != 32 || (r.Stage != StageExecute && r.Stage != StageClaim) {
		return ""
	}
	t.actionAttempt = r.Attempt
	if r.Binding != actionBinding(t, r.Stage) {
		return ""
	}
	return r.Stage
}
