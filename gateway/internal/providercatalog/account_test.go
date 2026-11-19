package providercatalog

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"workbuddy2api/internal/accountvault"
)

func TestAccountToggleRequiresRevisionAndExplicitState(t *testing.T) {
	v, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	a, err := v.Put(accountvault.Account{ID: "synthetic-account", Provider: "fixture", Enabled: true}, []byte(`{"synthetic":"private-session"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Accounts: v, Sources: []ProviderSource{fixtureSource{id: "fixture"}}})
	for _, tc := range []struct {
		body string
		code int
	}{{`{"id":"synthetic-account","revision":1}`, 400}, {`{"id":"synthetic-account","revision":1,"enabled":false}`, 200}, {`{"id":"synthetic-account","revision":1,"enabled":true}`, 409}} {
		w := httptest.NewRecorder()
		s.Route(w, httptest.NewRequest(http.MethodPost, "/workbench/account/toggle", strings.NewReader(tc.body)))
		if w.Code != tc.code {
			t.Fatalf("account mutation status=%d want=%d", w.Code, tc.code)
		}
		if strings.Contains(w.Body.String(), "private-session") {
			t.Fatal("account mutation leaked session")
		}
	}
	got, b, err := v.Get(a.ID)
	if err != nil || got.Enabled || got.Revision != 2 || !strings.Contains(string(b), "private-session") {
		t.Fatal("toggle changed secret or lost revision")
	}
}
