package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Effort struct {
	Level int    `json:"level"`
	Name  string `json:"name"`
}
type Model struct {
	ID            string   `json:"id"`
	UpstreamID    string   `json:"upstream_id"`
	ItemID        int64    `json:"item_id"`
	Name          string   `json:"name"`
	ModeID        string   `json:"mode_id"`
	ModeName      string   `json:"mode_name"`
	DefaultMode   string   `json:"default_mode"`
	ContextWindow int64    `json:"context_length,omitempty"`
	MemoryProfile string   `json:"memory_profile,omitempty"`
	NativeTools   bool     `json:"native_tools"`
	Efforts       []Effort `json:"reasoning_efforts,omitempty"`
	DefaultEffort int      `json:"default_effort,omitempty"`
	Tiers         []string `json:"tiers,omitempty"`
	NeedLogin     bool     `json:"need_login"`
}
type Catalog struct {
	Models    []Model   `json:"models"`
	FetchedAt time.Time `json:"fetched_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Stale     bool      `json:"stale"`
	ErrorCode string    `json:"error_code,omitempty"`
}
type catalogCache struct {
	snapshot Catalog
	revision string
	retryAt  time.Time
	loading  chan struct{}
	lastErr  error
}
type modelItem struct {
	ItemID      *int64 `json:"item_id"`
	Key         string `json:"model_item_key"`
	Name        string `json:"name"`
	DefaultMode string `json:"default_mode"`
	ShowTools   int    `json:"show_task_tools"`
	NeedLogin   bool   `json:"need_login"`
	Extra       struct {
		Window string `json:"total_window_size"`
		Memory string `json:"memory_profile"`
	} `json:"model_extra_params"`
	Effort struct {
		Default int      `json:"default_level"`
		Options []Effort `json:"option_list"`
	} `json:"reasoning_effort_config"`
	Tags []struct {
		Text string `json:"text"`
	} `json:"tag_list"`
}
type modelList struct {
	Items []modelItem `json:"item_list"`
}
type modeItem struct {
	ID     string    `json:"mode_id"`
	Name   string    `json:"name"`
	Models modelList `json:"model_list"`
}
type menuConfig struct {
	PerMode bool      `json:"use_mode_model_list"`
	Models  modelList `json:"model_list"`
	Modes   struct {
		Items []modeItem `json:"item_list"`
	} `json:"mode_list"`
}

func ParseCatalog(data []byte) ([]Model, error) {
	var body struct {
		Entries []struct {
			Switch struct {
				Menu *menuConfig `json:"menu_conf_v2"`
			} `json:"active_switch_conf"`
		} `json:"entry_list"`
	}
	if json.Unmarshal(data, &body) != nil {
		return nil, errors.New("invalid_doubao_catalog")
	}
	out := []Model{}
	seen := map[string]bool{}
	found := false
	add := func(items []modelItem, mode, name string) {
		for _, x := range items {
			itemID := int64(0)
			if x.ItemID != nil {
				itemID = *x.ItemID
				if x.Key == "" {
					x.Key = strconv.FormatInt(itemID, 10)
				}
			}
			if x.Key == "" || x.Name == "" {
				continue
			}
			id := mode + ":" + x.Key
			if seen[id] {
				continue
			}
			seen[id] = true
			window, _ := strconv.ParseInt(x.Extra.Window, 10, 64)
			if window < 0 {
				window = 0
			}
			m := Model{ID: "doubao:" + id, UpstreamID: x.Key, ItemID: itemID, Name: x.Name, ModeID: mode, ModeName: name, DefaultMode: x.DefaultMode, ContextWindow: window, MemoryProfile: x.Extra.Memory, NativeTools: x.ShowTools == 1, Efforts: x.Effort.Options, DefaultEffort: x.Effort.Default, NeedLogin: x.NeedLogin}
			for _, t := range x.Tags {
				if t.Text != "" {
					m.Tiers = append(m.Tiers, t.Text)
				}
			}
			out = append(out, m)
		}
	}
	for _, entry := range body.Entries {
		menu := entry.Switch.Menu
		if menu == nil {
			continue
		}
		found = true
		if menu.PerMode {
			for _, mode := range menu.Modes.Items {
				if strings.TrimSpace(mode.ID) == "" {
					return nil, errors.New("doubao_catalog_mode_missing")
				}
				add(mode.Models.Items, mode.ID, mode.Name)
			}
		} else {
			add(menu.Models.Items, "default", "")
		}
	}
	if !found || len(out) == 0 {
		return nil, errors.New("doubao_catalog_schema_changed")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func copyCatalog(s Catalog) Catalog {
	b, _ := json.Marshal(s)
	var out Catalog
	_ = json.Unmarshal(b, &out)
	return out
}

// Catalog coalesces concurrent refreshes per account and login revision. On a
// failure callers receive explicit stale metadata and an error, never a fallback
// model. Stale data older than one hour is withheld.
func (c *Client) Catalog(ctx context.Context, account string, force bool) (Catalog, error) {
	for {
		session, err := c.sessions.Session(ctx, account)
		if err != nil {
			if canceled := contextFailure(ctx, err); canceled != nil {
				return Catalog{}, canceled
			}
			return Catalog{}, &APIError{Kind: "login_required"}
		}
		c.mu.Lock()
		entry := c.cache[account]
		if entry == nil || entry.revision != session.Revision {
			entry = &catalogCache{revision: session.Revision}
			c.cache[account] = entry
		}
		if entry.loading != nil {
			done := entry.loading
			c.mu.Unlock()
			select {
			case <-done:
				force = false
				continue
			case <-ctx.Done():
				return Catalog{}, ctx.Err()
			}
		}
		now := time.Now()
		if !force && now.Before(entry.snapshot.ExpiresAt) {
			out := copyCatalog(entry.snapshot)
			c.mu.Unlock()
			return out, nil
		}
		if !force && now.Before(entry.retryAt) {
			out := copyCatalog(entry.snapshot)
			out.Stale = true
			out.ErrorCode = "refresh_failed"
			if now.Sub(out.FetchedAt) > time.Hour {
				out.Models = []Model{}
			}
			err := entry.lastErr
			c.mu.Unlock()
			return out, err
		}
		entry.loading = make(chan struct{})
		c.mu.Unlock()
		data, fetchErr := c.callSession(ctx, session, "/alice/slot/action_bar_v3/brief_list", map[string]any{"bot_id": session.BotID, "language_code": "zh"})
		var models []Model
		if fetchErr == nil {
			models, fetchErr = ParseCatalog(data)
		}
		c.mu.Lock()
		if fetchErr == nil {
			entry.snapshot = Catalog{Models: models, FetchedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
			entry.lastErr = nil
			entry.retryAt = time.Time{}
		} else if errors.Is(fetchErr, context.Canceled) || errors.Is(fetchErr, context.DeadlineExceeded) {
			entry.lastErr = nil
			entry.retryAt = time.Time{}
		} else {
			entry.lastErr = fetchErr
			entry.retryAt = time.Now().Add(15 * time.Second)
		}
		close(entry.loading)
		entry.loading = nil
		out := copyCatalog(entry.snapshot)
		c.mu.Unlock()
		if fetchErr != nil {
			out.Stale = true
			out.ErrorCode = "refresh_failed"
			if time.Since(out.FetchedAt) > time.Hour {
				out.Models = []Model{}
			}
		}
		return out, fetchErr
	}
}
