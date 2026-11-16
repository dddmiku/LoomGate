package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDesktopOutputBudgetAppliesToIOCopy(t *testing.T) {
	var output boundedOutput
	_, err := io.Copy(&output, io.LimitReader(strings.NewReader(strings.Repeat("x", 3<<20)), 3<<20))
	if err == nil || !output.exceeded || output.buffer.Len() > 2<<20 {
		t.Fatal("io.Copy bypassed the helper output limit")
	}
}

func TestDesktopHelperProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--desktop-fixture" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	var in runtimeRequest
	if json.NewDecoder(os.Stdin).Decode(&in) != nil {
		os.Exit(2)
	}
	switch mode {
	case "delay":
		time.Sleep(10 * time.Second)
	case "error":
		fmt.Fprint(os.Stdout, `{"ok":false,"error":"PRIVATE_SYNTHETIC_EXCEPTION"}`)
	case "stderr":
		fmt.Fprint(os.Stderr, "PRIVATE_SYNTHETIC_EXCEPTION")
		os.Exit(2)
	case "limit":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 3<<20))
	case "trailing":
		fmt.Fprint(os.Stdout, `{"ok":true} {}`)
	case "wrong-identity":
		_ = json.NewEncoder(os.Stdout).Encode(runtimeReply{OK: true, Identity: strings.Repeat("b", 64), Signed: true, Data: json.RawMessage(`{}`)})
	case "unsigned":
		_ = json.NewEncoder(os.Stdout).Encode(runtimeReply{OK: true, Identity: in.Identity, Data: json.RawMessage(`{}`)})
	case "success":
		if in.Operation == "identity" {
			_ = json.NewEncoder(os.Stdout).Encode(runtimeReply{OK: true, Identity: strings.Repeat("a", 64)})
		} else {
			var data struct {
				Data json.RawMessage `json:"data"`
			}
			_ = json.Unmarshal([]byte(catalogFixture(t, "Runtime model")), &data)
			_ = json.NewEncoder(os.Stdout).Encode(runtimeReply{OK: true, Identity: in.Identity, Signed: true, Data: data.Data})
		}
	}
	os.Exit(0)
}
func desktopFixture(t *testing.T, mode string) *CommandRuntime {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCommandRuntime([]string{exe, "-test.run=^TestDesktopHelperProcess$", "--", "--desktop-fixture", mode}, 19226)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestDesktopCommandBoundary(t *testing.T) {
	r := desktopFixture(t, "success")
	b, err := r.Bind(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.Catalog(context.Background(), b, "synthetic-bot")
	if err != nil {
		t.Fatal(err)
	}
	models, err := ParseCatalog(raw)
	if err != nil || len(models) != 1 || models[0].Name != "Runtime model" {
		t.Fatal("projected catalog contract failed")
	}
	for _, mode := range []string{"error", "stderr", "limit", "trailing", "wrong-identity", "unsigned"} {
		t.Run(mode, func(t *testing.T) {
			_, err := desktopFixture(t, mode).Catalog(context.Background(), b, "synthetic-bot")
			if err == nil || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("unsafe subprocess result accepted")
			}
		})
	}
	for _, bot := range []string{"", "../../anything", "https://example.invalid/"} {
		if _, err := r.Catalog(context.Background(), b, bot); err == nil {
			t.Fatal("invalid bot accepted")
		}
	}
	if _, err := r.Catalog(context.Background(), RuntimeBinding{Identity: "invalid"}, "synthetic-bot"); err == nil {
		t.Fatal("invalid binding accepted")
	}
}

func TestDesktopCompletionPreparationRejectsWithoutRuntimeAndInvalidInputs(t *testing.T) {
	s := sessionSourceFunc(func(context.Context, string) (Session, error) {
		return Session{BotID: "synthetic-bot", Runtime: &RuntimeBinding{Identity: strings.Repeat("a", 64)}, Revision: "1"}, nil
	})
	if _, err := NewWithRuntime(s, nil).PrepareCompletion(context.Background(), "account", "synthetic"); err == nil {
		t.Fatal("missing completion runtime accepted")
	}
	r := desktopFixture(t, "success")
	b := RuntimeBinding{Identity: strings.Repeat("a", 64)}
	for _, prompt := range []string{"", strings.Repeat("x", 8193)} {
		if _, err := r.PrepareCompletion(context.Background(), b, "synthetic-bot", prompt); err == nil {
			t.Fatal("invalid completion prompt accepted")
		}
	}
}
func TestDesktopCancellationStopsChildAndQueuedRead(t *testing.T) {
	r := desktopFixture(t, "delay")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Bind(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatal("child did not stop on deadline")
	}
	r.slot <- struct{}{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = r.Bind(ctx2)
	<-r.slot
	if !errors.Is(err, context.Canceled) {
		t.Fatal("queued cancellation lost")
	}
	if len(r.slot) != 0 {
		t.Fatal("slot leaked")
	}
}

type runtimeFunc func(context.Context, RuntimeBinding, string) (json.RawMessage, error)

func (f runtimeFunc) Catalog(ctx context.Context, b RuntimeBinding, bot string) (json.RawMessage, error) {
	return f(ctx, b, bot)
}
func TestDesktopAccountIsolationNoFallbackAndCacheInvalidation(t *testing.T) {
	b := &RuntimeBinding{Identity: strings.Repeat("a", 64)}
	var fail bool
	calls := 0
	s := sessionSourceFunc(func(_ context.Context, id string) (Session, error) {
		return Session{Runtime: b, BotID: "synthetic-bot", Revision: "1"}, nil
	})
	c := NewWithRuntime(s, runtimeFunc(func(_ context.Context, got RuntimeBinding, bot string) (json.RawMessage, error) {
		calls++
		if got.Identity != b.Identity || bot != "synthetic-bot" {
			t.Fatal("binding scope changed")
		}
		if fail {
			return nil, &APIError{Kind: "desktop_identity_mismatch"}
		}
		var v struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal([]byte(catalogFixture(t, "Bound model")), &v)
		return v.Data, nil
	}))
	c.http.Transport = syntheticTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("runtime fell back to captured credentials")
		return nil, nil
	})
	first, err := c.Catalog(context.Background(), "account-a", false)
	if err != nil || len(first.Models) != 1 {
		t.Fatal("initial catalog failed")
	}
	_, err = c.Catalog(context.Background(), "account-a", false)
	if err != nil || calls != 1 {
		t.Fatal("cache bypassed")
	}
	_, err = c.Catalog(context.Background(), "account-b", false)
	if err != nil || calls != 2 {
		t.Fatal("account cache crossed")
	}
	if _, err = c.call(context.Background(), "account-a", "/alice/ug/checkin", nil); err == nil || calls != 2 {
		t.Fatal("mutation reached desktop runtime")
	}
	fail = true
	last, err := c.Catalog(context.Background(), "account-a", true)
	if err == nil || len(last.Models) != 0 {
		t.Fatal("identity mismatch retained stale models")
	}
	_, err = c.Catalog(context.Background(), "account-a", true)
	if err == nil || calls != 3 {
		t.Fatal("backoff bypassed")
	}
	if _, err = NewWithRuntime(s, nil).Catalog(context.Background(), "account-a", false); err == nil {
		t.Fatal("missing runtime accepted")
	}
}
