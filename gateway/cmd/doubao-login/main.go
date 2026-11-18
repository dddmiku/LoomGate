// doubao-login is the gateway-side Doubao login console.
//
//	qr    -> request a QR code, print a scannable image, poll until confirmed
//	sms   -> send a verification code, then submit it
//
// It performs the real login against the live service and stores the resulting
// encrypted profile (including the discovered bot id) into the provider vault.
// It never prints cookies, tokens or phone numbers.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/providers/doubao"
)

func run() error {
	vaultDir := flag.String("vault", "", "provider-accounts vault directory")
	mode := flag.String("mode", "qr", "qr or sms")
	phone := flag.String("phone", "", "phone with country code for sms mode, e.g. '+86 13800000000'")
	code := flag.String("code", "", "verification code for sms mode")
	account := flag.String("account", "", "optional account id to overwrite")
	resume := flag.Bool("resume", false, "continue the newest unfinished flow instead of starting a new login")
	flag.Parse()

	if *vaultDir == "" {
		return fmt.Errorf("vault_required")
	}
	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		return fmt.Errorf("vault_open_failed")
	}
	defer vault.Close()

	driver := &doubao.Passport{Save: func(ctx context.Context, id string, profile doubao.StoredProfile) error {
		rows, err := vault.List()
		if err != nil {
			return err
		}
		var revision uint64
		for _, a := range rows {
			if a.ID == id {
				if a.Provider != "doubao" {
					return fmt.Errorf("account_provider_conflict")
				}
				revision = a.Revision
			}
		}
		raw, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		defer func() {
			for i := range raw {
				raw[i] = 0
			}
		}()
		_, err = vault.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: true}, raw, revision)
		return err
	}}
	service, err := authflow.Open(authflow.Options{Vault: vault, Drivers: map[string]authflow.Driver{"doubao": driver}})
	if err != nil {
		return fmt.Errorf("authentication_service_unavailable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var view authflow.View
	if *resume {
		// Recover an already-authenticated flow whose commit step failed (for
		// example an interrupted bot-id lookup). This performs a read-only
		// identity re-check, then an explicit commit; it never re-sends a login.
		rows, listErr := service.List()
		if listErr != nil {
			return fmt.Errorf("authentication_state_unavailable")
		}
		found := false
		for _, row := range rows {
			if row.Status == "verified" || row.Status == "manual_action" || row.Status == "uncertain" {
				view = row
				found = true
			}
		}
		if !found {
			return fmt.Errorf("resume_flow_not_found")
		}
		view, err = service.Step(ctx, view.ID, "resume", authflow.Input{})
		if err != nil {
			return fmt.Errorf("resume_failed: %w", err)
		}
	} else {
		view, err = service.Start(ctx, authflow.Request{Provider: "doubao", Method: *mode, AccountID: *account})
		if err != nil {
			return fmt.Errorf("start_failed: %w", err)
		}
	}
	emit := func(v authflow.View, extra map[string]any) {
		row := map[string]any{
			"status":     v.Status,
			"error_code": v.ErrorCode,
			"notice":     v.Notice,
			"account_id": v.AccountID,
		}
		if v.QRURL != "" {
			row["qr_url"] = v.QRURL
		}
		if v.RetryAt.After(time.Now()) {
			row["retry_after_s"] = int(time.Until(v.RetryAt).Seconds())
		}
		for k, val := range extra {
			row[k] = val
		}
		out, _ := json.Marshal(row)
		fmt.Println(string(out))
	}
	emit(view, nil)

	switch *mode {
	case "qr":
		// Poll the scan state until it resolves. Polling is a read-only request.
		// The service throttles polls via NextPollAt, so honour it instead of
		// hammering and getting authentication_busy.
		for i := 0; i < 200; i++ {
			if view.Status != "waiting_scan" && view.Status != "scanned" {
				break
			}
			wait := 4 * time.Second
			if d := time.Until(view.NextPollAt); d > 0 {
				wait = d + 300*time.Millisecond
			}
			time.Sleep(wait)
			next, stepErr := service.Step(ctx, view.ID, "poll", authflow.Input{})
			if stepErr != nil {
				// A throttle race is not a failure; retry after the window.
				if stepErr == authflow.ErrBusy {
					time.Sleep(2 * time.Second)
					continue
				}
				return fmt.Errorf("poll_failed: %w", stepErr)
			}
			view = next
			emit(view, nil)
		}
	case "sms":
		if view.Status == "waiting_phone" {
			if *phone == "" {
				return fmt.Errorf("phone_required")
			}
			view, err = service.Step(ctx, view.ID, "send_code", authflow.Input{Phone: *phone})
			if err != nil {
				return fmt.Errorf("send_code_failed: %w", err)
			}
			emit(view, nil)
		}
		if view.Status == "waiting_code" {
			if *code == "" {
				return fmt.Errorf("code_required")
			}
			view, err = service.Step(ctx, view.ID, "submit_code", authflow.Input{Code: *code})
			if err != nil {
				return fmt.Errorf("submit_code_failed: %w", err)
			}
			emit(view, nil)
		}
	}
	if view.Status == "verified" {
		view, err = service.Step(ctx, view.ID, "commit", authflow.Input{})
		if err != nil {
			return fmt.Errorf("commit_failed: %w", err)
		}
		emit(view, nil)
	}
	if view.Status != "succeeded" {
		return fmt.Errorf("login_not_completed: status=%s code=%s", view.Status, view.ErrorCode)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
