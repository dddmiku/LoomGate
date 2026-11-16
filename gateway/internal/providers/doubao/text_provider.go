package doubao

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao/stream"
)

type TextProvider struct {
	Vault   *accountvault.Vault
	Client  *Client
	Runtime *CommandRuntime
}

func (p *TextProvider) account(ctx context.Context) (accountvault.Account, Session, error) {
	bound, err := p.Runtime.Bind(ctx)
	if err != nil {
		return accountvault.Account{}, Session{}, err
	}
	rows, err := p.Vault.List()
	if err != nil {
		return accountvault.Account{}, Session{}, runtimeError("desktop_binding_invalid")
	}
	for _, a := range rows {
		if a.Provider != "doubao" || !a.Enabled {
			continue
		}
		s, err := (VaultSessions{p.Vault}).Session(ctx, a.ID)
		if err == nil && s.Runtime != nil && s.Runtime.Identity == bound.Identity {
			return a, s, nil
		}
	}
	return accountvault.Account{}, Session{}, runtimeError("desktop_identity_mismatch")
}
func (p *TextProvider) Models(ctx context.Context) ([]inference.Model, error) {
	a, _, err := p.account(ctx)
	if err != nil {
		return nil, err
	}
	c, err := p.Client.Catalog(ctx, a.ID, false)
	if err != nil {
		return nil, err
	}
	if c.Stale {
		return nil, runtimeError("desktop_model_unavailable")
	}
	models := []inference.Model{}
	for _, m := range c.Models {
		if !m.NativeTools {
			models = append(models, inference.Model{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow})
		}
	}
	return models, nil
}
func (p *TextProvider) Complete(ctx context.Context, model, text string) (inference.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	a, s, err := p.account(ctx)
	if err != nil {
		return inference.Result{}, err
	}
	result := inference.Result{AccountRef: fmt.Sprintf("doubao-%x", sha256.Sum256([]byte(a.ID)))[:19]}
	prepared, err := p.Runtime.PrepareText(ctx, *s.Runtime, model, text)
	if err != nil {
		return result, err
	}
	defer prepared.Close()
	if err = p.verifyAccount(ctx, a.ID, s.Revision); err != nil {
		return result, err
	}
	blocks := map[string]string{}
	order := []string{}
	known := map[string]bool{}
	size := 0
	report, err := (stream.Runner{Opener: prepared, Options: stream.Options{Timeout: 90 * time.Second, AttemptTimeout: 30 * time.Second, MaxResumes: 1}}).Run(ctx, prepared.Body, func(u stream.Update) error {
		key := u.MessageID + "/" + u.BlockID
		if u.Operation == 3 {
			size -= len(blocks[key])
			delete(blocks, key)
			delete(known, key)
			kept := order[:0]
			for _, k := range order {
				if k != key {
					kept = append(kept, k)
				}
			}
			order = kept
			return nil
		}
		if u.Kind != "text" {
			return nil
		}
		if !known[key] {
			known[key] = true
			order = append(order, key)
		}
		if u.Operation == 1 {
			blocks[key] += u.Text
			size += len(u.Text)
		} else {
			size -= len(blocks[key])
			blocks[key] = u.Text
			size += len(u.Text)
		}
		if size > 8<<20 {
			return stream.ErrLimit
		}
		return nil
	})
	result.RequestDigest = prepared.RequestDigest
	result.Attempted = prepared.Submitted
	result.Acknowledged = report.Acknowledged
	result.Complete = report.ProtocolComplete
	result.Events = report.Events
	result.Attempts = report.Attempts
	result.AttemptRecords = append([]inference.Attempt(nil), prepared.Attempts...)
	if err != nil {
		return result, err
	}
	if err = prepared.VerifyIdentity(ctx); err != nil {
		return result, err
	}
	if err = p.verifyAccount(ctx, a.ID, s.Revision); err != nil {
		return result, err
	}
	if err = prepared.Close(); err != nil {
		return result, runtimeError("desktop_cleanup_uncertain")
	}
	var output strings.Builder
	for _, key := range order {
		output.WriteString(blocks[key])
	}
	result.Text = output.String()
	if !result.Complete || !result.Acknowledged || strings.TrimSpace(result.Text) == "" {
		return result, stream.ErrIncomplete
	}
	return result, nil
}

func (p *TextProvider) verifyAccount(ctx context.Context, id, revision string) error {
	current, err := (VaultSessions{p.Vault}).Session(ctx, id)
	if err != nil || current.Revision != revision {
		return runtimeError("desktop_binding_invalid")
	}
	return nil
}
