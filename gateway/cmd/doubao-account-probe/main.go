// doubao-account-probe reports which non-secret identity fields the passport
// account-info endpoint exposes, so the account list can show a human-readable
// label instead of a bare internal id. It prints field names and whether a value
// is already masked; it never prints a phone number, email or id value.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

func run() error {
	vaultDir := flag.String("vault", "", "provider-accounts vault directory")
	flag.Parse()
	if *vaultDir == "" {
		return fmt.Errorf("vault_required")
	}
	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		return fmt.Errorf("vault_open_failed")
	}
	defer vault.Close()

	rows, err := vault.List()
	if err != nil {
		return fmt.Errorf("vault_list_failed")
	}
	var target string
	for _, a := range rows {
		if a.Provider == "doubao" && a.Enabled {
			target = a.ID
		}
	}
	if target == "" {
		return fmt.Errorf("no_doubao_account")
	}

	sessions := doubao.VaultSessions{Vault: vault}
	s, err := sessions.Session(context.Background(), target)
	if err != nil {
		return fmt.Errorf("session_unavailable")
	}
	query := url.Values{}
	for k, v := range s.Query {
		query[k] = v
	}
	query.Del("real_aid")
	query.Del("a_bogus")
	query.Del("msToken")
	u := url.URL{Scheme: "https", Host: "accounts.doubao.com", Path: "/passport/account/info/v2/", RawQuery: query.Encode()}
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header = s.Headers.Clone()
	req.Header.Del("Cookie")
	if cookie := s.Headers.Get("Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request_failed")
	}
	defer resp.Body.Close()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if dec.Decode(&envelope) != nil {
		return fmt.Errorf("response_invalid")
	}
	keys := make([]string, 0, len(envelope.Data))
	for k := range envelope.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("http=%d fields=%d\n", resp.StatusCode, len(keys))
	for _, k := range keys {
		shape := shapeOf(envelope.Data[k])
		if shape != "" {
			fmt.Printf("  %-28s %s\n", k, shape)
		}
	}
	return nil
}

// shapeOf classifies a value without revealing it.
func shapeOf(raw any) string {
	switch value := raw.(type) {
	case string:
		if value == "" {
			return "empty"
		}
		switch {
		case strings.Contains(value, "*"):
			return fmt.Sprintf("masked(len=%d)", len(value))
		case looksMaskedPhone(value):
			return fmt.Sprintf("phone-shaped(len=%d)", len(value))
		case strings.Contains(value, "@"):
			return "email-shaped"
		default:
			return fmt.Sprintf("text(len=%d)", len(value))
		}
	case json.Number:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "object" + fmt.Sprint(keys)
	case []any:
		return fmt.Sprintf("array(len=%d)", len(value))
	}
	return ""
}

func looksMaskedPhone(value string) bool {
	if len(value) < 7 || len(value) > 20 {
		return false
	}
	digits := 0
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits++
		} else if r != '+' && r != '-' && r != ' ' {
			return false
		}
	}
	return digits >= 7
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
