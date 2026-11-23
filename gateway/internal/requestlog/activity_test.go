package requestlog

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAccountActivityCountsActualAttemptsAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	now := time.Now().UTC()
	r := fixtureRecord("fixture-retry")
	r.Model = "autoclaw:fixture"
	r.StartedAt = now.Add(-time.Minute)
	r.FinishedAt = now.Add(-30 * time.Second)
	r.Attempts = []Attempt{{Number: 1, AccountID: "fixture-a", StartedAt: r.StartedAt, FinishedAt: r.StartedAt.Add(time.Second), Status: StatusError, ErrorCode: "rate_limited"}, {Number: 2, AccountID: "fixture-b", StartedAt: r.StartedAt.Add(2 * time.Second), FinishedAt: r.FinishedAt, Status: StatusSuccess, HTTPStatus: 200}}
	appendFixture(t, s, r)
	old := fixtureRecord("fixture-old")
	old.Model = r.Model
	old.StartedAt = now.Add(-25 * time.Hour)
	old.FinishedAt = old.StartedAt.Add(time.Second)
	old.Attempts = []Attempt{{Number: 1, AccountID: "fixture-a", StartedAt: old.StartedAt, FinishedAt: old.FinishedAt, Status: StatusSuccess}}
	appendFixture(t, s, old)
	missing := fixtureRecord("fixture-no-account")
	missing.Model = r.Model
	missing.Attempts = []Attempt{{Number: 1, StartedAt: now.Add(-time.Second), Status: StatusError}}
	appendFixture(t, s, missing)
	check := func(store *Store) {
		t.Helper()
		p, e := store.AccountActivity("autoclaw")
		if e != nil || len(p.Items) != 2 || !p.Partial {
			t.Fatal("actual account coverage not preserved")
		}
		byID := map[string]AccountActivity{}
		for _, a := range p.Items {
			byID[a.AccountID] = a
		}
		a, b := byID["fixture-a"], byID["fixture-b"]
		if a.Attempts24H != 1 || a.Error24H != 1 || a.Success24H != 0 || a.LastRequestID != r.RequestID || b.Success24H != 1 || b.Attempts24H != 1 {
			t.Fatal("retries, last activity or 24h scope were misattributed")
		}
		empty, e := store.AccountActivity("workbuddy")
		if e != nil || len(empty.Items) != 0 {
			t.Fatal("provider scope mixed account identities")
		}
	}
	check(s)
	second := openFixture(t, path, Options{})
	check(second)
}
