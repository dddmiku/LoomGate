package taskproviders

import (
	"context"
	"path/filepath"
	"testing"
	"workbuddy2api/internal/taskengine"
)

// The adapter and engine under test are production code. Only the synthetic
// upstream changes between the engine's inspection and Claim's final read.
type productionReceiptRegressionAdapter struct {
	*Accounts
	fixture          *lifecycleFixture
	disappear        bool
	actionPhase      string
	acceptedAtVerify taskengine.Stage
}

func (a *productionReceiptRegressionAdapter) Claim(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
	if a.disappear {
		a.fixture.mu.Lock()
		a.fixture.travel = "idle"
		a.fixture.record = 0
		a.fixture.mu.Unlock()
	}
	result, err := a.Accounts.Claim(ctx, task)
	a.actionPhase = result.Phase
	return result, err
}
func (a *productionReceiptRegressionAdapter) Verify(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
	a.acceptedAtVerify = task.AcceptedStage
	return a.Accounts.Verify(ctx, task)
}
func TestProductionReceiptRequiresActualAcceptedMutation(t *testing.T) {
	for _, disappear := range []bool{false, true} {
		name := "real_accepted_receipt_control"
		if disappear {
			name = "record_disappears_before_claim_preflight"
		}
		t.Run(name, func(t *testing.T) {
			accounts, f, _ := lifecycleSetup(t)
			f.travel, f.record = "arrived", 901
			adapter := &productionReceiptRegressionAdapter{Accounts: accounts, fixture: f, disappear: disappear}
			engine, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: 1, PollMax: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			if err := engine.Register("workbuddy", adapter); err != nil {
				t.Fatal(err)
			}
			run := runLifecycle(t, engine, "travel/claim")
			f.mu.Lock()
			posts := f.counts["/activity/growth/buddy/travel/claim"]
			f.mu.Unlock()
			t.Logf("claim_posts=%d action_phase=%s accepted_stage=%s run_status=%s", posts, adapter.actionPhase, adapter.acceptedAtVerify, run.Status)
			if !disappear {
				if posts != 1 || run.Status != taskengine.StatusSucceeded {
					t.Fatal("control: valid accepted claim did not settle")
				}
				return
			}
			if posts != 0 {
				t.Fatal("fixture did not exercise the no-submission path")
			}
			if adapter.acceptedAtVerify != "" {
				t.Errorf("no mutation was sent, but engine manufactured accepted_stage=%s", adapter.acceptedAtVerify)
			}
			if run.Status == taskengine.StatusSucceeded {
				t.Error("no accepted claim receipt and missing record were falsely confirmed as successful reward settlement")
			}
		})
	}
}
