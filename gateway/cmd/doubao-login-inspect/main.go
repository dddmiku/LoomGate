// doubao-login-inspect performs one explicit authentication step, retaining
// resumable private state in the encrypted vault. It prints no QR link or token.
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

func main() {
	profile := flag.String("profile", "", "protected official SDK context file")
	state := flag.String("state", "", "encrypted authentication vault directory")
	method := flag.String("method", "qr", "qr or sms; sms does not send a message without explicit input")
	newFlow := flag.Bool("new", false, "explicitly start a new flow after reviewing previous terminal state")
	flag.Parse()
	if *profile == "" || *state == "" {
		fmt.Println("authentication_arguments_required")
		os.Exit(2)
	}
	v, err := accountvault.Open(*state)
	if err != nil {
		fmt.Println("authentication_vault_unavailable")
		os.Exit(1)
	}
	defer v.Close()
	p := &doubao.Passport{Bootstrap: func(context.Context) (doubao.StoredProfile, error) {
		var p doubao.StoredProfile
		b, e := os.ReadFile(*profile)
		if e != nil {
			return p, e
		}
		defer func() {
			for i := range b {
				b[i] = 0
			}
		}()
		e = json.Unmarshal(b, &p)
		return p, e
	}}
	s, err := authflow.Open(authflow.Options{Vault: v, Drivers: map[string]authflow.Driver{"doubao": p}})
	if err != nil {
		fmt.Println("authentication_service_unavailable")
		os.Exit(1)
	}
	rows, err := s.List()
	if err != nil {
		fmt.Println("authentication_state_unavailable")
		os.Exit(1)
	}
	for _, r := range rows {
		if r.Status != "expired" && r.Status != "failed" && r.Status != "canceled" && r.Status != "succeeded" {
			printView(r, true)
			return
		}
	}
	if len(rows) > 0 && !*newFlow {
		printView(rows[len(rows)-1], true)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := s.Start(ctx, authflow.Request{Provider: "doubao", Method: *method})
	if err != nil {
		fmt.Println("authentication_start_unavailable")
		os.Exit(1)
	}
	printView(r, false)
}
func printView(v authflow.View, resumed bool) {
	b, _ := json.Marshal(map[string]any{"status": v.Status, "error_code": v.ErrorCode, "method": v.Method, "resumed": resumed, "qr_available": v.QRURL != "", "phone_submitted": false, "code_submitted": false, "expires_at": v.ExpiresAt})
	fmt.Println(string(b))
}
