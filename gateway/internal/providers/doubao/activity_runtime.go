package doubao

import (
	"context"
	"encoding/json"
	"strings"
)

type ActivityRuntime interface {
	Activities(context.Context, RuntimeBinding) (Activity, error)
	TaskAction(context.Context, RuntimeBinding, string, string, string, string) (Activity, error)
}

func (r *CommandRuntime) Activities(ctx context.Context, b RuntimeBinding) (Activity, error) {
	return r.taskOperation(ctx, b, "activities", "", "", "")
}
func (r *CommandRuntime) TaskAction(ctx context.Context, b RuntimeBinding, operation, group, task, instance string) (Activity, error) {
	if operation != "task_checkin" && operation != "task_claim" {
		return Activity{}, runtimeError("desktop_protocol_invalid")
	}
	if strings.TrimSpace(group) == "" || strings.TrimSpace(task) == "" {
		return Activity{}, runtimeError("desktop_binding_invalid")
	}
	return r.taskOperation(ctx, b, operation, group, task, instance)
}
func (r *CommandRuntime) taskOperation(ctx context.Context, b RuntimeBinding, operation, group, task, instance string) (Activity, error) {
	if !identityDigest.MatchString(b.Identity) {
		return Activity{}, runtimeError("desktop_binding_invalid")
	}
	reply, err := r.run(ctx, runtimeRequest{Operation: operation, Port: r.port, Identity: b.Identity, Group: group, Task: task, Instance: instance})
	if err != nil {
		return Activity{}, err
	}
	var activity Activity
	if reply.Identity != b.Identity || json.Unmarshal(reply.Data, &activity) != nil || activity.Group == "" || activity.Tasks == nil || len(activity.Tasks) > 256 || len(activity.Milestones) > 64 {
		return Activity{}, runtimeError("desktop_protocol_invalid")
	}
	for _, t := range append(append([]Task{}, activity.Tasks...), activity.Milestones...) {
		if t.Group != activity.Group || strings.TrimSpace(t.Code) == "" {
			return Activity{}, runtimeError("desktop_protocol_invalid")
		}
	}
	return activity, nil
}
func (c *Client) TaskAction(ctx context.Context, account, operation string, t Task) (Activity, error) {
	return c.TaskActionRevision(ctx, account, "", operation, t)
}
func (c *Client) TaskActionRevision(ctx context.Context, account, revision, operation string, t Task) (Activity, error) {
	s, err := c.sessions.Session(ctx, account)
	if err != nil {
		return Activity{}, runtimeError("login_required")
	}
	if revision != "" && s.Revision != revision {
		return Activity{}, runtimeError("desktop_binding_invalid")
	}
	if s.Runtime != nil {
		r, ok := c.runtime.(ActivityRuntime)
		if !ok {
			return Activity{}, runtimeError("desktop_activity_unavailable")
		}
		return r.TaskAction(ctx, *s.Runtime, operation, t.Group, t.Code, t.ID)
	}
	if operation == "task_checkin" {
		if err := c.Checkin(ctx, account, t.Group); err != nil {
			return Activity{}, err
		}
		return c.Activities(ctx, account)
	}
	return Activity{}, runtimeError("desktop_task_manual")
}
