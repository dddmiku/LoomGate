package taskproviders

// Shared durable-attempt plumbing for tasks that consume a one-shot resource
// (lottery chance, Buddy energy). Task files own the business rules; this file
// only owns the write-ahead journal: the pre-spend snapshot, the stable client
// token and the accepted/unknown receipt must survive a crash so a restart can
// only verify, never re-spend.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

type spendRewardModule struct {
	moduleBase
	up     *upstream.Client
	vault  *accountvault.Vault
	mu     sync.Mutex
	memory map[string]string
}

type spendJournal struct {
	Version     int       `json:"version"`
	Kind        string    `json:"kind"`
	Account     string    `json:"account"`
	Period      string    `json:"period"`
	Reference   string    `json:"reference"`
	Stage       string    `json:"stage"`
	State       string    `json:"state"`
	Before      int       `json:"before"`
	After       int       `json:"after,omitempty"`
	ClientToken string    `json:"client_token"`
	Summary     string    `json:"summary,omitempty"`
	Started     time.Time `json:"started"`
	Updated     time.Time `json:"updated"`
}

func spendVaultID(account, kind string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + kind))
	return "spend-" + hex.EncodeToString(sum[:])
}

func (m *spendRewardModule) loadSpend(a *auth.Auth, kind string) (spendJournal, uint64, error) {
	var value spendJournal
	if m.vault == nil {
		return value, 0, errors.New("spend_store_unavailable")
	}
	meta, raw, err := m.vault.Get(spendVaultID(a.Snapshot().UID, kind))
	if err != nil {
		return value, 0, err
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	if json.Unmarshal(raw, &value) != nil || value.Version != 1 || value.Kind != kind || value.Account != a.Snapshot().UID {
		return value, 0, errors.New("spend_store_invalid")
	}
	return value, meta.Revision, nil
}

func (m *spendRewardModule) saveSpend(a *auth.Auth, value spendJournal, rev uint64) (uint64, error) {
	if m.vault == nil {
		return 0, errors.New("spend_store_unavailable")
	}
	value.Version = 1
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, errors.New("spend_store_invalid")
	}
	meta, err := m.vault.Put(accountvault.Account{ID: spendVaultID(a.Snapshot().UID, value.Kind), Provider: "workbuddy-spend", Enabled: true}, raw, rev)
	return meta.Revision, err
}

func (m *spendRewardModule) currentPeriod(a *auth.Auth) string {
	return time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

func (m *spendRewardModule) settledToday(a *auth.Auth, kind string) (string, bool, error) {
	j, _, err := m.loadSpend(a, kind)
	if err != nil {
		if errors.Is(err, accountvault.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if j.Period != m.currentPeriod(a) || j.State != "accepted" {
		return "", false, nil
	}
	return j.Summary, true, nil
}

func (m *spendRewardModule) beginSpend(a *auth.Auth, kind, period, stage, reference string, before int) (spendJournal, uint64, error) {
	if reference == "" {
		return spendJournal{}, 0, errors.New("spend_attempt_missing")
	}
	old, rev, err := m.loadSpend(a, kind)
	if err != nil && !errors.Is(err, accountvault.ErrNotFound) {
		return spendJournal{}, 0, err
	}
	if err == nil && old.Period == period {
		switch old.State {
		case "accepted", "unknown", "attempting", "unresolved":
			return old, rev, errors.New("spend_previous_attempt_unresolved")
		case "not_submitted":
			// Explicit provider refusal proved no effect; a later chance is
			// allowed to create a new attempt on top of the same record.
		default:
			return old, rev, errors.New("spend_state_invalid")
		}
	}
	value := spendJournal{Version: 1, Kind: kind, Account: a.Snapshot().UID, Period: period, Reference: reference, Stage: stage, State: "attempting", Before: before, ClientToken: kind + "-" + reference[:32], Started: time.Now().UTC(), Updated: time.Now().UTC()}
	rev, err = m.saveSpend(a, value, rev)
	return value, rev, err
}

func (m *spendRewardModule) finishSpend(a *auth.Auth, value spendJournal, rev uint64, state, summary string, after int) error {
	value.State = state
	value.Summary = summary
	value.After = after
	value.Updated = time.Now().UTC()
	_, err := m.saveSpend(a, value, rev)
	return err
}

// discoverSpend turns a single-definition spend task into a catalog row with its
// authoritative state. The period key is the CST business day, matching the
// journal period.
func discoverSpend(m interface {
	Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
	Spec() ModuleSpec
	Discover(context.Context, *auth.Auth) ([]taskengine.Definition, error)
}, ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	state, err := m.Inspect(ctx, a, taskengine.Task{Definition: taskengine.Definition{}})
	if err != nil {
		return nil, err
	}
	defs, err := m.Discover(ctx, a)
	if err != nil || len(defs) != 1 {
		return nil, errors.New("spend_definition_invalid")
	}
	def := defs[0]
	def.SideEffect = true
	def.OncePerDay = true
	def.MaxAttempts = 1
	def.PeriodKey = time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	def.Stages = []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}
	return []ModuleTask{{Definition: def, State: state}}, nil
}

var _ = fmt.Sprintf
