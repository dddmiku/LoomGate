package taskproviders

import (
	"context"
	"path/filepath"
	"testing"
	"workbuddy2api/internal/taskengine"
)

type preflightChangeAdapter struct {
	*Accounts
	fixture *lifecycleFixture
	key     string
}

func (a *preflightChangeAdapter) change() {
	a.fixture.mu.Lock()
	defer a.fixture.mu.Unlock()
	switch a.key {
	case "travel/depart":
		a.fixture.daily = true
	case "redeem/future-plan":
		a.fixture.tier = "claimed"
	case "makeup":
		a.fixture.made = []string{"2099-01-02"}
	case "compensation":
		a.fixture.claimed = true
	}
}
func (a *preflightChangeAdapter) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	a.change()
	return a.Accounts.Execute(ctx, t)
}
func (a *preflightChangeAdapter) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	a.change()
	return a.Accounts.Claim(ctx, t)
}
func TestAllLifecyclePreflightNoOpsHaveNoMutationReceiptOrPermanentLock(t *testing.T) {
	for _, key := range []string{"travel/depart", "redeem/future-plan", "makeup", "compensation"} {
		t.Run(key, func(t *testing.T) {
			accounts, f, _ := lifecycleSetup(t)
			adapter := &preflightChangeAdapter{Accounts: accounts, fixture: f, key: key}
			e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json")})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if err = e.Register("workbuddy", adapter); err != nil {
				t.Fatal(err)
			}
			r := runLifecycle(t, e, key)
			if r.Status != taskengine.StatusSkipped {
				t.Fatal("no-op was reported as accepted success or unknown")
			}
			for _, step := range r.Steps {
				if step.Stage == taskengine.StageExecute || step.Stage == taskengine.StageClaim {
					if step.ActionOutcome != "not_submitted" {
						t.Fatal("no-op lost explicit disposition")
					}
				}
			}
			catalog, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"synthetic-cn"}}, key)
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range catalog.Tasks {
				if task.PendingVerification || task.AcceptedStage != "" {
					t.Fatal("no-op created persistent receipt/lock")
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.writeBodies) != 0 {
				t.Fatal("no-op preflight still sent a request")
			}
		})
	}
}
