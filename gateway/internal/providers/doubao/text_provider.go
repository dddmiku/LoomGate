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
			models = append(models, inference.Model{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow, Incremental: true})
		}
	}
	return models, nil
}
func (p *TextProvider) Complete(ctx context.Context, model, text string) (inference.Result, error) {
	return p.complete(ctx, model, text, nil)
}
func (p *TextProvider) Stream(ctx context.Context, model, text string, emit func(string) error) (inference.Result, error) {
	if emit == nil {
		return inference.Result{}, inference.ErrStreamContract
	}
	return p.complete(ctx, model, text, emit)
}
func (p *TextProvider) complete(ctx context.Context, model, text string, emit func(string) error) (inference.Result, error) {
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
	progress := textProgress{}
	if emit != nil {
		progress.emit = func(delta string) error {
			if err := p.verifyAccount(ctx, a.ID, s.Revision); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return emit(delta)
		}
	}
	report, err := (stream.Runner{Opener: prepared, Options: stream.Options{Timeout: 90 * time.Second, AttemptTimeout: 30 * time.Second, MaxResumes: 1}}).RunObserved(ctx, prepared.Body, progress.apply)

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
	result.Text = progress.text
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
