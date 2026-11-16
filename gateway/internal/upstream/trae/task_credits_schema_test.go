package trae

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTaskCreditsRejectsIncompleteNestedSnapshot(t *testing.T) {
	for _, mode := range []string{"missing_base", "missing_extra", "missing_package", "missing_quota", "quota_string", "limit_overflow", "limit_negative", "missing_usage", "null_usage", "renamed_usage", "usage_invalid", "missing_time", "time_object", "time_fraction", "time_overflow", "fallback_invalid", "summary_overflow", "valid_empty", "valid_fallback", "valid_no_expiry"} {
		t.Run(mode, func(t *testing.T) {
			quota := map[string]any{"credits_limit": 500}
			pe := map[string]any{"quota": quota}
			extra := map[string]any{"package_extra": pe}
			base := map[string]any{"product_extra": extra, "end_time": time.Now().Add(time.Hour).Unix()}
			usage := map[string]any{"credits_amount": 100}
			pack := map[string]any{"entitlement_base_info": base, "usage": usage}
			summary := map[string]any{"total_amount": 1000, "consumed_amount": 200}
			switch mode {
			case "missing_base":
				delete(pack, "entitlement_base_info")
			case "missing_extra":
				delete(base, "product_extra")
			case "missing_package":
				delete(extra, "package_extra")
			case "missing_quota":
				delete(pe, "quota")
			case "quota_string":
				quota["credits_limit"] = "500"
			case "limit_overflow":
				quota["credits_limit"] = float64(math.MaxInt64)
			case "limit_negative":
				quota["credits_limit"] = -1
			case "missing_usage":
				delete(pack, "usage")
			case "null_usage":
				pack["usage"] = nil
			case "renamed_usage":
				delete(usage, "credits_amount")
				usage["changed"] = 100
			case "usage_invalid":
				usage["credits_amount"] = []any{100}
			case "missing_time":
				delete(base, "end_time")
			case "time_object":
				base["end_time"] = map[string]any{}
			case "time_fraction":
				base["end_time"] = 1.2
			case "time_overflow":
				base["end_time"] = float64(math.MaxInt64)
			case "fallback_invalid":
				pack["expire_time"] = "invalid"
			case "summary_overflow":
				summary["total_amount"] = float64(math.MaxInt64)
			case "valid_empty":
				pack["usage"] = map[string]any{}
			case "valid_fallback":
				base["end_time"] = 0
				pack["expire_time"] = time.Now().Add(time.Hour).Unix()
			case "valid_no_expiry":
				base["end_time"] = 0
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"code": 0, "usage_summary": summary, "user_entitlement_pack_list": []any{pack}})
			}))
			defer server.Close()
			got, err := (&TaskClient{HTTP: server.Client(), Origin: server.URL}).Credits(context.Background(), syntheticTaskAuth())
			valid := mode == "valid_empty" || mode == "valid_fallback" || mode == "valid_no_expiry"
			if valid {
				if err != nil || got.Remaining != 800 || len(got.Packs) != 1 {
					t.Fatal("valid snapshot rejected")
				}
			} else if err == nil {
				t.Fatal("incomplete snapshot accepted")
			}
		})
	}
}

func TestTaskExpiryCapsWithoutOverflowAndRejectsBadSnapshot(t *testing.T) {
	now := time.Now()
	c := Credits{Total: 800, Remaining: 800, Packs: []CreditPack{{Limit: 8e18, Remaining: 8e18, EndTime: now.Add(time.Hour).Unix()}, {Limit: 8e18, Remaining: 8e18, EndTime: now.Add(time.Hour).Unix()}}}
	got, err := TaskExpiry(c, 2*time.Hour, now)
	if err != nil || got != 800 {
		t.Fatal("bucket exceeded balance or overflowed")
	}
	c.Packs[0].Remaining = math.Inf(1)
	if _, err = TaskExpiry(c, time.Hour, now); err == nil {
		t.Fatal("non-finite snapshot accepted")
	}
}
