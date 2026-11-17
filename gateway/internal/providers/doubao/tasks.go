package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type Reward struct {
	Amount int64  `json:"reward_amount"`
	Type   string `json:"reward_type"`
}
type DailyCheckin struct {
	Checked      bool   `json:"checked_in_today"`
	Date         string `json:"last_checkin_date,omitempty"`
	Reward       Reward `json:"reward"`
	RewardStatus int    `json:"reward_status,omitempty"`
}
type Task struct {
	ID              string          `json:"task_id,omitempty"`
	Code            string          `json:"campaign_code"`
	Group           string          `json:"campaign_group_code"`
	Kind            string          `json:"task_kind"`
	Title           string          `json:"title"`
	Subtitle        string          `json:"subtitle"`
	Description     string          `json:"description"`
	Type            string          `json:"campaign_type"`
	Recommendations []string        `json:"recommend_prompts,omitempty"`
	Status          int             `json:"task_status"`
	RewardMode      int             `json:"reward_mode"`
	Daily           *DailyCheckin   `json:"daily_checkin,omitempty"`
	Rewards         []Reward        `json:"reward_summaries,omitempty"`
	Threshold       int64           `json:"milestone_threshold,omitempty"`
	Progress        *TaskProgress   `json:"progress,omitempty"`
	Work            *WorkCapability `json:"work_capability,omitempty"`
}
type WorkCapability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}
type TaskProgress struct {
	Current int64 `json:"current"`
	Target  int64 `json:"target"`
}
type RewardRecord struct {
	ID        string `json:"record_id"`
	TaskCode  string `json:"task_code"`
	Amount    string `json:"amount"`
	Unit      string `json:"unit"`
	CreatedMS int64  `json:"create_time_ms"`
}

func (RewardRecord) String() string   { return "doubao.RewardRecord[redacted]" }
func (RewardRecord) GoString() string { return "doubao.RewardRecord[redacted]" }

type RewardPage struct {
	Records []RewardRecord `json:"records"`
	HasMore bool           `json:"has_more"`
}

// Checkin only accepts a group discovered for this account in the current
// execution. Completion still requires a subsequent Activities read-back.
func (c *Client) Checkin(ctx context.Context, account, group string) error {
	if strings.TrimSpace(group) == "" {
		return errors.New("doubao_activity_identity_missing")
	}
	data, err := c.call(ctx, account, "/alice/ug/campaign/landing/checkin", map[string]any{"scene": group})
	if err != nil {
		return err
	}
	var result struct {
		Checked *bool `json:"checked_in"`
	}
	if json.Unmarshal(data, &result) != nil || result.Checked == nil || !*result.Checked {
		return errors.New("doubao_checkin_unconfirmed")
	}
	return nil
}

type Activity struct {
	Group           string `json:"group"`
	Tasks           []Task `json:"tasks"`
	Milestones      []Task `json:"milestones"`
	TotalAmount     int64  `json:"total_amount"`
	OperationStatus string `json:"operation_status,omitempty"`
}

func (c *Client) Activities(ctx context.Context, account string) (Activity, error) {
	session, err := c.sessions.Session(ctx, account)
	if err != nil {
		if canceled := contextFailure(ctx, err); canceled != nil {
			return Activity{}, canceled
		}
		return Activity{}, &APIError{Kind: "login_required"}
	}
	if session.Runtime != nil {
		runtime, ok := c.runtime.(ActivityRuntime)
		if !ok {
			return Activity{}, &APIError{Kind: "desktop_activity_unavailable"}
		}
		return runtime.Activities(ctx, *session.Runtime)
	}
	basic, err := c.callSession(ctx, session, "/alice/basic/launch", map[string]any{})
	if err != nil {
		return Activity{}, err
	}
	var launch struct {
		Campaign struct {
			Group string `json:"campaign_group_code"`
		} `json:"ug_campaign_info"`
	}
	if json.Unmarshal(basic, &launch) != nil || launch.Campaign.Group == "" {
		return Activity{}, errors.New("doubao_activity_unavailable")
	}
	data, err := c.callSession(ctx, session, "/alice/ug/campaign/v3/detail/", map[string]any{"campaign_group_code": launch.Campaign.Group})
	if err != nil {
		return Activity{}, err
	}
	var detail struct {
		Campaign   string `json:"campaign_code"`
		Tasks      []Task `json:"tasks"`
		Milestones []Task `json:"milestones"`
		Progress   struct {
			Total int64 `json:"total_amount"`
		} `json:"progress"`
	}
	if json.Unmarshal(data, &detail) != nil || detail.Tasks == nil {
		return Activity{}, errors.New("doubao_activity_schema_changed")
	}
	if detail.Campaign != "" && detail.Campaign != launch.Campaign.Group {
		return Activity{}, errors.New("doubao_activity_identity_mismatch")
	}
	out := Activity{Group: launch.Campaign.Group, Tasks: detail.Tasks, Milestones: detail.Milestones, TotalAmount: detail.Progress.Total}
	for _, t := range append(append([]Task{}, out.Tasks...), out.Milestones...) {
		if strings.TrimSpace(t.Code) == "" || t.Group != out.Group {
			return Activity{}, errors.New("doubao_activity_identity_mismatch")
		}
	}
	return out, nil
}
