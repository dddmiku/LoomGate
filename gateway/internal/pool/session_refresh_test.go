package pool

import (
	"path/filepath"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
)

func TestSessionRefreshPreservesIndependentRestrictions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := New(path)
	defer p.Close()
	a := &auth.Auth{UID: "synthetic-refresh"}
	p.Add(a)
	p.Cooldown(a.UID, CoolHard, time.Hour, "synthetic balance exhausted")
	p.CooldownSoftForModel(a.UID, time.Hour, time.Now().Add(time.Hour), "synthetic-model", "synthetic model limit")
	before, _ := p.Status(a.UID)
	if !p.CompleteSessionRefresh(a.UID, a) {
		t.Fatal("current identity rejected")
	}
	after, _ := p.Status(a.UID)
	if !after.CreditsExhausted || after.Reason != before.Reason || !after.Until.Equal(before.Until) {
		t.Fatal("refresh changed unrelated credit restriction")
	}
	p.Flush()
	reopened := New(path)
	defer reopened.Close()
	reopened.Add(a)
	after, _ = reopened.Status(a.UID)
	if !after.CreditsExhausted || after.Reason != before.Reason {
		t.Fatal("refresh cleared persisted credit restriction")
	}
	if reopened.PickExcludingForRealm(nil, "synthetic-model", "cn") != nil {
		t.Fatal("refresh released unrelated model restriction")
	}
}
