package autoclaw

import (
	"sort"
	"time"
	"workbuddy2api/internal/accountvault"
)

// Selection is strictly cache-only: it never starts a wallet read or refresh.
// Fresh provider-reported expiring points get priority; equal-tier accounts
// retain the existing round-robin order. Missing/stale/error facts are unknown.
func (p *Provider) prioritizeExpiring(order []accountvault.Account) int {
	if p.Vault == nil {
		return 0
	}
	now := time.Now()
	priority := make(map[string]bool, len(order))
	keys := make(map[string]string, len(order))
	for _, row := range order {
		keys[row.ID] = p.Vault.OpaqueID("credit-cache", row.ID)
	}
	autoClawCreditCache.Lock()
	for _, row := range order {
		entry, ok := autoClawCreditCache.rows[keys[row.ID]]
		if !ok || entry.at.IsZero() || now.Sub(entry.at) > creditCacheTTL || entry.at.After(now) {
			continue
		}
		v := entry.value
		priority[row.ID] = v.Supported && !v.Pending && v.ErrorCode == "" && v.Remain != nil && *v.Remain > 0 && v.Expiring != nil && *v.Expiring > 0 && *v.Expiring <= *v.Remain
	}
	autoClawCreditCache.Unlock()
	sort.SliceStable(order, func(i, j int) bool { return priority[order[i].ID] && !priority[order[j].ID] })
	count := 0
	for _, row := range order {
		if priority[row.ID] {
			count++
		}
	}
	return count
}
func (p *Provider) orderAccounts(rows []accountvault.Account, start int) []accountvault.Account {
	ordered := append([]accountvault.Account(nil), rows...)
	preferred := p.prioritizeExpiring(ordered)
	result := make([]accountvault.Account, len(rows))
	if start < 0 {
		start = 0
	}
	rotate := func(from, to int) {
		n := to - from
		if n == 0 {
			return
		}
		offset := start % n
		for i := 0; i < n; i++ {
			result[from+i] = ordered[from+(offset+i)%n]
		}
	}
	if preferred == 0 {
		rotate(0, len(rows))
	} else {
		rotate(0, preferred)
		rotate(preferred, len(rows))
	}
	return result
}
