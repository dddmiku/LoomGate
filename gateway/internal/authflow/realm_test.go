package authflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type realmDriver struct {
	methods, calls int
	received       Request
}

func (d *realmDriver) Methods() []Method { d.methods++; return []Method{{ID: "qr", Available: true}} }
func (d *realmDriver) Start(_ context.Context, r Request) (Result, error) {
	d.calls++
	d.received = r
	return Result{Status: "waiting_scan"}, nil
}
func (d *realmDriver) Step(context.Context, Flow, string, Input) (Result, error) {
	d.calls++
	return Result{Status: "verified"}, nil
}

func TestRealmContractBeforeDriverOrPersistence(t *testing.T) {
	for _, provider := range []string{"workbuddy", "trae", "autoclaw"} {
		for _, realm := range []string{"", "cn", "global", "trae", "other", "CN", " cn"} {
			for _, httpBoundary := range []bool{false, true} {
				name := provider + "/" + realm + "/service"
				if httpBoundary {
					name = provider + "/" + realm + "/http"
				}
				t.Run(name, func(t *testing.T) {
					d := &realmDriver{}
					s, _, path, _ := testService(t, d)
					s.opts.Drivers = map[string]Driver{provider: d}
					r := Request{Provider: provider, Method: "qr", Realm: realm}
					valid := provider == "workbuddy" && (realm == "" || realm == "cn" || realm == "global") || provider != "workbuddy" && realm == ""
					var got View
					var err error
					if httpBoundary {
						raw, _ := json.Marshal(r)
						w := httptest.NewRecorder()
						s.Route(w, httptest.NewRequest(http.MethodPost, "/api/connections/start", strings.NewReader(string(raw))))
						want := 400
						if valid {
							want = 200
						}
						if w.Code != want {
							t.Fatalf("status=%d want=%d", w.Code, want)
						}
						var result struct {
							Flow View `json:"flow"`
						}
						if json.Unmarshal(w.Body.Bytes(), &result) != nil {
							t.Fatal("invalid HTTP result")
						}
						got = result.Flow
					} else {
						got, err = s.Start(context.Background(), r)
						if !valid && err != ErrInvalid {
							t.Fatalf("illegal realm error=%v", err)
						}
						if valid && err != nil {
							t.Fatal(err)
						}
					}
					if !valid {
						if d.methods != 0 || d.calls != 0 || s.record.Revision != 0 || len(s.flows) != 0 {
							t.Fatal("invalid realm consulted driver or mutated state")
						}
						if _, err := os.Stat(filepath.Join(path, "authentication-flows.account")); !os.IsNotExist(err) {
							t.Fatal("invalid realm persisted flow")
						}
						return
					}
					wantRealm := realm
					if provider == "workbuddy" && realm == "" {
						wantRealm = "cn"
					}
					if d.calls != 1 || d.received.Realm != wantRealm || got.Realm != wantRealm || got.Status != "waiting_scan" {
						t.Fatal("valid realm not preserved")
					}
				})
			}
		}
	}
}
func TestStoredForeignRealmCannotResumeDriver(t *testing.T) {
	d := &realmDriver{}
	s, _, _, now := testService(t, d)
	s.opts.Drivers = map[string]Driver{"workbuddy": d}
	f := Flow{View: View{ID: "synthetic-foreign-flow", Request: Request{Provider: "workbuddy", Method: "qr", Realm: "trae"}, Status: "waiting_scan", ExpiresAt: now.Add(60e9), Revision: 1}}
	if err := s.save([]Flow{f}); err != nil {
		t.Fatal(err)
	}
	revision := s.record.Revision
	if _, err := s.Step(context.Background(), f.ID, "poll", Input{}); err != ErrInvalid || d.calls != 0 || s.record.Revision != revision {
		t.Fatal("persisted foreign realm resumed")
	}
	if _, err := s.Step(context.Background(), f.ID, "cancel", Input{}); err != nil {
		t.Fatal("invalid historical flow must remain cancelable")
	}
}
