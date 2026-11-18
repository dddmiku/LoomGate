// doubao-capture-import writes one encrypted Doubao account profile from a
// private capture of the official desktop client.
//
// Input is the completion capture produced by the read-only CDP listener:
//   { "request": { "url": ..., "headers": {...}, "post_data": "{...}" } }
//
// It stores:
//   - Query: the official client's device parameters (signatures stripped,
//     because the service does not enforce them)
//   - Headers: the request headers that carry session cookies
//   - BotID: client_meta.bot_id from the captured body
//   - CompletionTemplate: the captured body itself
//
// Never prints cookie values, signed query values, or body contents.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

// requestHeaderAllowlist keeps only headers that are protocol-relevant. Trace
// ids and per-request ids are regenerated; lengths and cookies are recomputed.
var requestHeaderAllowlist = map[string]bool{
	"accept":                     true,
	"accept-language":            true,
	"agw-js-conv":                true,
	"referer":                    true,
	"user-agent":                 true,
	"sec-ch-ua":                  true,
	"sec-ch-ua-mobile":           true,
	"sec-ch-ua-platform":         true,
	"x-lgw-req-sdk-type":         true,
	"x-net-sdk-domain-dispatch":  true,
	"x-ttnet-bypass-decompression": true,
	"cookie":                     true,
}

type captureFile struct {
	Request struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Post    string            `json:"post_data"`
	} `json:"request"`
}

func run() error {
	capturePath := flag.String("capture", "", "private completion capture JSON")
	vaultDir := flag.String("vault", "", "provider-accounts vault directory")
	accountID := flag.String("account", "", "local account id (optional; derived from device_id when empty)")
	enabled := flag.Bool("enabled", true, "mark the imported account enabled")
	flag.Parse()

	if *capturePath == "" || *vaultDir == "" {
		return errors.New("capture_and_vault_required")
	}
	info, err := os.Lstat(*capturePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return errors.New("capture_unreadable")
	}
	raw, err := os.ReadFile(*capturePath)
	if err != nil {
		return errors.New("capture_unreadable")
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()

	var cap captureFile
	if json.Unmarshal(raw, &cap) != nil {
		return errors.New("capture_invalid")
	}
	if cap.Request.URL == "" || cap.Request.Post == "" {
		return errors.New("capture_incomplete")
	}

	u, err := url.Parse(cap.Request.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "www.doubao.com" || u.User != nil {
		return errors.New("capture_origin_invalid")
	}
	if u.Path != "/chat/completion" {
		return errors.New("capture_endpoint_invalid")
	}

	query := u.Query()
	query.Del("a_bogus")
	query.Del("msToken")

	headers := http.Header{}
	for key, value := range cap.Request.Headers {
		lower := strings.ToLower(key)
		if !requestHeaderAllowlist[lower] {
			continue
		}
		if lower == "cookie" && value == "" {
			continue
		}
		headers.Set(http.CanonicalHeaderKey(lower), value)
	}
	if headers.Get("Cookie") == "" {
		return errors.New("capture_missing_session_cookie")
	}

	var body struct {
		Client struct {
			BotID string `json:"bot_id"`
		} `json:"client_meta"`
		Messages []json.RawMessage `json:"messages"`
		Option   struct {
			Unique string `json:"unique_key"`
		} `json:"option"`
	}
	if json.Unmarshal([]byte(cap.Request.Post), &body) != nil {
		return errors.New("capture_body_invalid")
	}
	if body.Client.BotID == "" || len(body.Messages) != 1 || body.Option.Unique == "" {
		return errors.New("capture_body_shape_invalid")
	}

	id := *accountID
	if id == "" {
		deviceID := query.Get("device_id")
		if deviceID == "" {
			return errors.New("capture_missing_device_id")
		}
		sum := sha256.Sum256([]byte("doubao-device\x00" + deviceID))
		id = "doubao-" + hex.EncodeToString(sum[:8])
	}

	profile := doubao.StoredProfile{
		Query:              query,
		Headers:            headers,
		BotID:              body.Client.BotID,
		CompletionTemplate: json.RawMessage(cap.Request.Post),
		BindingFlow:        "official-pc-capture",
		VerifiedAt:         time.Now().UTC(),
	}

	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		return fmt.Errorf("vault_open_failed: %w", err)
	}
	defer vault.Close()
	rows, err := vault.List()
	if err != nil {
		return errors.New("vault_list_failed")
	}
	var revision uint64
	found := false
	for _, a := range rows {
		if a.ID != id {
			continue
		}
		if a.Provider != "doubao" {
			return errors.New("account_provider_conflict")
		}
		revision = a.Revision
		found = true
	}

	secret, err := json.Marshal(profile)
	if err != nil {
		return errors.New("profile_encode_failed")
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	stored, err := vault.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: *enabled}, secret, revision)
	if err != nil {
		return fmt.Errorf("vault_put_failed: %w", err)
	}

	// Prove the stored profile can actually serve a catalog listing. Read-only.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := doubao.New(doubao.VaultSessions{Vault: vault}, nil)
	catalog, catErr := client.Catalog(ctx, id, true)
	modelCount := len(catalog.Models)
	stale := catalog.Stale

	out := map[string]any{
		"account_id":         stored.ID,
		"revision":           stored.Revision,
		"created":            !found,
		"enabled":            stored.Enabled,
		"bot_id_present":     profile.BotID != "",
		"device_params":      len(query),
		"header_count":       len(headers),
		"cookie_present":     headers.Get("Cookie") != "",
		"template_bytes":     len(profile.CompletionTemplate),
		"catalog_ok":         catErr == nil,
		"catalog_models":     modelCount,
		"catalog_stale":      stale,
		"secrets_printed":    0,
	}
	if catErr != nil {
		out["catalog_error"] = catErr.Error()
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	return enc.Encode(out)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
