package doubao

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

type RewardRuntime interface {
	RewardRecords(context.Context, RuntimeBinding) (RewardPage, error)
}

func (r *CommandRuntime) RewardRecords(ctx context.Context, b RuntimeBinding) (RewardPage, error) {
	if !identityDigest.MatchString(b.Identity) {
		return RewardPage{}, runtimeError("desktop_binding_invalid")
	}
	reply, err := r.run(ctx, runtimeRequest{Operation: "reward_records", Port: r.port, Identity: b.Identity})
	if err != nil {
		return RewardPage{}, err
	}
	var page RewardPage
	if reply.Identity != b.Identity || json.Unmarshal(reply.Data, &page) != nil || page.Records == nil || len(page.Records) > 100 {
		return page, runtimeError("desktop_protocol_invalid")
	}
	for _, record := range page.Records {
		amount, e := strconv.ParseInt(record.Amount, 10, 64)
		if record.ID == "" || len(record.ID) > 128 || record.TaskCode == "" || len(record.TaskCode) > 128 || record.CreatedMS <= 0 || e != nil || amount < 0 || strings.ContainsAny(record.Amount, "+- ") || len(record.Unit) > 32 {
			return RewardPage{}, runtimeError("desktop_protocol_invalid")
		}
	}
	return page, nil
}
func (c *Client) RewardRecords(ctx context.Context, account string) (RewardPage, error) {
	session, err := c.sessions.Session(ctx, account)
	if err != nil {
		return RewardPage{}, runtimeError("login_required")
	}
	if session.Runtime == nil {
		return RewardPage{}, runtimeError("desktop_activity_unavailable")
	}
	runtime, ok := c.runtime.(RewardRuntime)
	if !ok {
		return RewardPage{}, runtimeError("desktop_activity_unavailable")
	}
	return runtime.RewardRecords(ctx, *session.Runtime)
}
