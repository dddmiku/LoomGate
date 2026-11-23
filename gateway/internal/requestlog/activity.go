package requestlog

import (
	"sort"
	"strings"
	"time"
)

// Only actual retained upstream attempts contribute. Wallet reads are absent.
type AccountActivity struct {
	AccountID         string    `json:"account_id"`
	Provider          string    `json:"provider"`
	LastStartedAt     time.Time `json:"last_started_at"`
	LastFinishedAt    time.Time `json:"last_finished_at"`
	LastRequestID     string    `json:"last_request_id"`
	LastModel         string    `json:"last_model"`
	LastStatus        Status    `json:"last_status"`
	LastRequestStatus Status    `json:"last_request_status"`
	LastErrorCode     string    `json:"last_error_code,omitempty"`
	LastHTTPStatus    int       `json:"last_http_status,omitempty"`
	Attempts24H       int       `json:"attempts_24h"`
	Success24H        int       `json:"success_24h"`
	Error24H          int       `json:"error_24h"`
	Canceled24H       int       `json:"canceled_24h"`
}
type ActivityPage struct {
	Items         []AccountActivity `json:"items"`
	WindowSeconds int               `json:"window_seconds"`
	Partial       bool              `json:"partial"`
}
type activitySample struct {
	account, provider, request, model, code string
	start, finish                           time.Time
	status                                  Status
	requestStatus                           Status
	http                                    int
}

func activityProvider(model string) string {
	prefix, _, _ := strings.Cut(model, ":")
	if prefix == "cn" || prefix == "global" {
		return "workbuddy"
	}
	return prefix
}
func activitySamples(r Record) []activitySample {
	out := make([]activitySample, 0, len(r.Attempts))
	for _, a := range r.Attempts {
		if a.AccountID == "" || a.StartedAt.IsZero() {
			continue
		}
		model := a.Model
		if model == "" {
			model = r.Model
		}
		out = append(out, activitySample{a.AccountID, activityProvider(model), r.RequestID, model, a.ErrorCode, a.StartedAt, a.FinishedAt, a.Status, r.Status, a.HTTPStatus})
	}
	return out
}
func (s *Store) AccountActivity(provider string) (ActivityPage, error) {
	page := ActivityPage{Items: []AccountActivity{}, WindowSeconds: 86400}
	err := s.locked(func() error {
		if err := s.refreshLocked(false); err != nil {
			return err
		}
		if err := s.retainLocked(); err != nil {
			return err
		}
		cutoff := s.now().Add(-24 * time.Hour)
		if len(s.state.entries) >= s.options.MaxRecords || s.state.offset >= s.options.MaxBytes-MaxRecordBytes {
			page.Partial = true
		}
		byID := make(map[string]*AccountActivity)
		for _, e := range s.state.entries {
			if e.summary.AttemptsTruncated || e.summary.AttemptCount > len(e.activity) || s.state.header.Recovery != nil {
				page.Partial = true
			}
			for _, a := range e.activity {
				if provider != "" && a.provider != provider {
					continue
				}
				k := a.provider + "\x00" + a.account
				row := byID[k]
				if row == nil {
					row = &AccountActivity{AccountID: a.account, Provider: a.provider}
					byID[k] = row
				}
				if row.LastStartedAt.IsZero() || !activityAt(a.start, a.finish).Before(activityAt(row.LastStartedAt, row.LastFinishedAt)) {
					row.LastStartedAt, row.LastFinishedAt, row.LastRequestID, row.LastModel = a.start, a.finish, a.request, a.model
					row.LastStatus, row.LastErrorCode, row.LastHTTPStatus = a.status, a.code, a.http
					row.LastRequestStatus = a.requestStatus
				}
				if a.start.Before(cutoff) {
					continue
				}
				row.Attempts24H++
				switch a.status {
				case StatusSuccess:
					row.Success24H++
				case StatusCanceled:
					row.Canceled24H++
				default:
					row.Error24H++
				}
			}
		}
		for _, row := range byID {
			page.Items = append(page.Items, *row)
		}
		sort.Slice(page.Items, func(i, j int) bool {
			if !activityAt(page.Items[i].LastStartedAt, page.Items[i].LastFinishedAt).Equal(activityAt(page.Items[j].LastStartedAt, page.Items[j].LastFinishedAt)) {
				return activityAt(page.Items[i].LastStartedAt, page.Items[i].LastFinishedAt).After(activityAt(page.Items[j].LastStartedAt, page.Items[j].LastFinishedAt))
			}
			return page.Items[i].Provider+"\x00"+page.Items[i].AccountID < page.Items[j].Provider+"\x00"+page.Items[j].AccountID
		})
		if len(page.Items) > 1000 {
			page.Items = page.Items[:1000]
			page.Partial = true
		}
		return nil
	})
	return page, err
}
func activityAt(start, finish time.Time) time.Time {
	if !finish.IsZero() {
		return finish
	}
	return start
}
