package hotupdate

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartApplyReservesBeforeBlockedVisibility(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusForbidden)
	}))
	defer api.Close()
	defer close(release)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m := NewManager(Options{Enabled: true, Repo: "fixture/loomgate", Proxy: "-", Dir: t.TempDir(), Listener: listener, APIBase: api.URL})
	accepted, err := m.StartApply("")
	if err != nil || accepted.State != StateChecking {
		t.Fatal("submission did not atomically reserve checking before acknowledgement")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("visibility fixture was not entered")
	}
	if _, err = m.StartApply(""); err == nil {
		t.Fatal("duplicate asynchronous submission was accepted")
	}
	if _, err = m.Apply(context.Background(), ""); err == nil {
		t.Fatal("duplicate synchronous submission was accepted")
	}
	if m.Status().State != StateChecking || calls.Load() != 1 {
		t.Fatal("duplicate submission disturbed the reserved operation")
	}
}
