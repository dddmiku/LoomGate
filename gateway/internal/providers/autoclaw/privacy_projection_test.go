package autoclaw

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providercatalog"
)

func TestLegacyPhoneIDIsPrivateAcrossCatalogLoginAndToggle(t *testing.T) {
	const phone = "13900002222" // Synthetic fixture, not an account.
	legacy := "autoclaw-" + phone
	vault := mustProviderVault(t)
	profile, _ := json.Marshal(stored{Phone: phone, AccessToken: "fixture-access", RefreshToken: "fixture-refresh"})
	if _, err := vault.Put(accountvault.Account{ID: legacy, Provider: "autoclaw", Enabled: true}, profile, 0); err != nil {
		t.Fatal(err)
	}
	service := providercatalog.New(providercatalog.Options{Accounts: vault, Sources: []providercatalog.ProviderSource{Source{Vault: vault}}})
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		if !service.Route(rec, httptest.NewRequest(method, path, bytes.NewReader(body))) {
			t.Fatal("catalog route not handled")
		}
		if strings.Contains(rec.Body.String(), phone) {
			t.Fatal("private phone reached the public management response")
		}
		return rec
	}
	list := request(http.MethodGet, "/workbench/accounts", nil)
	var result struct{ Accounts []providercatalog.AccountView `json:"accounts"` }
	if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &result) != nil || len(result.Accounts) != 1 {
		t.Fatal("account view missing")
	}
	publicID := result.Accounts[0].ID
	if publicID == "" || publicID == legacy || result.Accounts[0].Mobile != "139****2222" {
		t.Fatal("legacy identity must be opaque but still show the masked mobile")
	}
	models := request(http.MethodGet, "/workbench/models?provider=autoclaw&account="+publicID, nil)
	var page providercatalog.Page
	if models.Code != 200 || json.Unmarshal(models.Body.Bytes(), &page) != nil || len(page.Catalogs) != 1 || page.Catalogs[0].AccountID != publicID {
		t.Fatal("public alias could not resolve the real account catalog")
	}
	loginID, err := SaveProfile(vault)(t.Context(), profile)
	if err != nil || loginID != publicID {
		t.Fatal("re-login must return the same public identity")
	}
	physical, _, err := vault.Get(legacy)
	if err != nil {
		t.Fatal("legacy physical row was moved or lost")
	}
	payload, _ := json.Marshal(map[string]any{"id": publicID, "enabled": false, "revision": physical.Revision})
	toggled := request(http.MethodPost, "/workbench/account/toggle", payload)
	if toggled.Code != 200 {
		t.Fatal("public alias could not update the existing row")
	}
	physical, _, err = vault.Get(legacy)
	if err != nil || physical.Enabled {
		t.Fatal("toggle did not reach the original account")
	}
	rows, err := vault.List()
	if err != nil || len(rows) != 1 {
		t.Fatal("privacy projection duplicated or removed a stored account")
	}
}

func TestNewAccountIDsAreVaultScopedAndStable(t *testing.T) {
	profile, _ := json.Marshal(stored{Phone: "13800001111", AccessToken: "fixture-access", RefreshToken: "fixture-refresh"})
	one := mustProviderVault(t)
	two := mustProviderVault(t)
	id1, err := SaveProfile(one)(t.Context(), profile)
	if err != nil { t.Fatal(err) }
	id2, err := SaveProfile(two)(t.Context(), profile)
	if err != nil { t.Fatal(err) }
	if id1 == id2 {
		t.Fatal("plain phone digest is enumerable and correlates accounts between vaults")
	}
	again, err := SaveProfile(one)(t.Context(), profile)
	if err != nil || again != id1 {
		t.Fatal("same-vault re-login changed the account identity")
	}
}
