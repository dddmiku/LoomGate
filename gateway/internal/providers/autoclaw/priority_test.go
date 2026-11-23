package autoclaw

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providercatalog"
)

func TestAutoClawPoolPrefersObservedExpiringCreditsWithoutWalletRequests(t *testing.T) {
	resetCreditCacheForTest(t)
	v := mustProviderVault(t)
	for _, id := range []string{"fixture-account-a", "fixture-account-b"} {
		raw, _ := json.Marshal(stored{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", DeviceID: "acg-fixture-device"})
		if _, err := v.Put(accountvault.Account{ID: id, Provider: "autoclaw", Enabled: true}, raw, 0); err != nil {
			t.Fatal(err)
		}
	}
	remain, expiring := int64(12000), int64(10000)
	now := time.Now()
	key := v.OpaqueID("credit-cache", "fixture-account-b")
	autoClawCreditCache.Lock()
	autoClawCreditCache.rows[key] = creditCacheEntry{at: now, value: providercatalog.CreditView{Supported: true, Remain: &remain, Expiring: &expiring, FetchedAt: &now}}
	autoClawCreditCache.Unlock()
	calls := 0
	p := &Provider{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != chatPath {
			t.Fatal("selection refreshed wallet or used an undeclared endpoint")
		}
		return creditResponse(r, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})}}
	q := inference.Dialogue{Model: "autoclaw:zai_auto", Messages: []inference.Message{{Role: "user", Content: "fixture"}}}
	result, err := p.CompleteDialogue(t.Context(), q, nil)
	if err != nil {
		t.Fatal("offline model fixture failed")
	}
	if result.AccountRef != publicAccountID(v, "fixture-account-b") || calls != 1 {
		t.Fatal("account pool ignored fresh observed expiring credits")
	}
}
func TestAutoClawPriorityRotatesWithinTierWithoutLowerTierBias(t *testing.T) {
	resetCreditCacheForTest(t)
	v := mustProviderVault(t)
	rows := []accountvault.Account{}
	now := time.Now()
	remain, expiring := int64(12000), int64(10000)
	for _, id := range []string{"fixture-a", "fixture-b", "fixture-c"} {
		rows = append(rows, accountvault.Account{ID: id})
		if id != "fixture-a" {
			autoClawCreditCache.Lock()
			autoClawCreditCache.rows[v.OpaqueID("credit-cache", id)] = creditCacheEntry{at: now, value: providercatalog.CreditView{Supported: true, Remain: &remain, Expiring: &expiring}}
			autoClawCreditCache.Unlock()
		}
	}
	p := &Provider{Vault: v}
	counts := map[string]int{}
	for i := 0; i < 12; i++ {
		order := p.orderAccounts(rows, i)
		counts[order[0].ID]++
		if order[2].ID != "fixture-a" {
			t.Fatal("non-expiring account displaced a known expiring candidate")
		}
	}
	if counts["fixture-b"] != 6 || counts["fixture-c"] != 6 || counts["fixture-a"] != 0 {
		t.Fatal("priority tier inherited bias from unrelated lower-tier accounts")
	}
}

func TestAutoClawPriorityKeepsExplicitScopeAndIgnoresUnusableFacts(t *testing.T) {
	for _, mode := range []string{"stale", "pending", "error", "zero", "disabled", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			resetCreditCacheForTest(t)
			v := mustProviderVault(t)
			for _, id := range []string{"fixture-a", "fixture-b"} {
				raw, _ := json.Marshal(stored{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", DeviceID: "acg-fixture-device"})
				if _, e := v.Put(accountvault.Account{ID: id, Provider: "autoclaw", Enabled: mode != "disabled" || id != "fixture-b"}, raw, 0); e != nil {
					t.Fatal(e)
				}
			}
			remain, expiring := int64(12000), int64(10000)
			now := time.Now()
			entry := creditCacheEntry{at: now, value: providercatalog.CreditView{Supported: true, Remain: &remain, Expiring: &expiring, FetchedAt: &now}}
			switch mode {
			case "stale":
				entry.at = now.Add(-2 * time.Minute)
			case "pending":
				entry.value.Pending = true
			case "error":
				entry.value.ErrorCode = "fixture_error"
			case "zero":
				expiring = 0
			}
			autoClawCreditCache.Lock()
			autoClawCreditCache.rows[v.OpaqueID("credit-cache", "fixture-b")] = entry
			autoClawCreditCache.Unlock()
			calls := 0
			p := &Provider{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != chatPath {
					t.Fatal("priority initiated a wallet/refresh request")
				}
				return creditResponse(r, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			})}}
			q := inference.Dialogue{Model: "autoclaw:zai_auto", Messages: []inference.Message{{Role: "user", Content: "fixture"}}}
			if mode == "explicit" {
				q.AccountID = publicAccountID(v, "fixture-a")
			}
			result, e := p.CompleteDialogue(t.Context(), q, nil)
			if e != nil || calls != 1 || result.AccountRef != publicAccountID(v, "fixture-a") {
				t.Fatal("priority overrode explicit account, disabled state or trusted stale/invalid credit observations")
			}
		})
	}
}
