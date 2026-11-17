package doubao

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao/stream"
)

type TextProvider struct {
	Vault     *accountvault.Vault
	Client    *Client
	Runtime   *CommandRuntime
	Dialogues *DialogueStore
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
		if m.NativeTools {
			continue
		}
		models = append(models, inference.Model{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow, Incremental: true, MultiTurn: p.Dialogues != nil})
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
	result, _, err := p.completeConversation(ctx, model, text, "", "", "", emit)
	return result, err
}
func (p *TextProvider) CompleteDialogue(ctx context.Context, q inference.Dialogue, emit func(string) error) (inference.Result, error) {
	return p.completeDialogue(ctx, q, emit, nil)
}
func (p *TextProvider) completeDialogue(ctx context.Context, q inference.Dialogue, emit func(string) error, task *taskAction) (inference.Result, error) {
	if err := validateDialogue(q); err != nil {
		return inference.Result{}, err
	}
	if p.Dialogues == nil {
		if len(q.Messages) != 1 || q.ConversationRef != "" {
			return inference.Result{}, dialogueError("provider_conversation_unavailable")
		}
		return p.complete(ctx, q.Model, q.Messages[0].Content, emit)
	}
	a, s, err := p.account(ctx)
	if err != nil {
		return inference.Result{}, err
	}
	if q.AccountID != "" && (a.ID != q.AccountID || s.Revision != q.AccountRevision) {
		return inference.Result{}, runtimeError("desktop_identity_mismatch")
	}
	turn, err := p.Dialogues.begin(q, a.ID, s.Revision)
	if err != nil {
		return inference.Result{}, err
	}
	result, conversation, err := p.completeConversation(ctx, q.Model, q.Messages[len(q.Messages)-1].Content, turn.Record.Conversation, a.ID, s.Revision, emit, task)
	result.ConversationRef = turn.ID
	// Saving an official cursor is part of completion, not a best-effort cache.
	if saveErr := p.Dialogues.finish(turn, q, result, conversation, err == nil && result.Complete && result.Acknowledged); saveErr != nil {
		return result, dialogueError("provider_conversation_save_uncertain")
	}
	return result, err
}
func (p *TextProvider) completeConversation(ctx context.Context, model, text, conversation, expectedAccount, expectedRevision string, emit func(string) error, task ...*taskAction) (inference.Result, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	a, s, err := p.account(ctx)
	if err != nil {
		return inference.Result{}, "", err
	}
	if expectedAccount != "" && (a.ID != expectedAccount || s.Revision != expectedRevision) {
		return inference.Result{}, "", runtimeError("desktop_identity_mismatch")
	}
	result := inference.Result{AccountRef: fmt.Sprintf("doubao-%x", sha256.Sum256([]byte(a.ID)))[:19]}
	native := len(task) > 0 && task[0] != nil
	// Official work preparation may create a sandbox/session. Journal it as a
	// potentially effectful step even if no completion request was observed.
	result.Attempted = native
	prepared, err := p.Runtime.prepareWork(ctx, *s.Runtime, model, text, conversation, native, task...)
	if err != nil {
		return result, "", err
	}
	defer prepared.Close()
	var body struct {
		Option struct {
			Unique string `json:"unique_key"`
		} `json:"option"`
		Messages []struct {
			Local string `json:"local_message_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(prepared.Body, &body) == nil && body.Option.Unique != "" && len(body.Messages) == 1 {
		result.Recovery, _ = json.Marshal(workCursor{Session: body.Option.Unique, Message: body.Messages[0].Local})
	}
	if err = p.verifyAccount(ctx, a.ID, s.Revision); err != nil {
		return result, "", err
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
	options := stream.Options{Timeout: 90 * time.Second, AttemptTimeout: 30 * time.Second, MaxResumes: 1}
	if native {
		options.Timeout = 10 * time.Minute
		options.AttemptTimeout = 120 * time.Second
		options.MaxResumes = 4
		options.TaskWork = true
	}
	report, err := (stream.Runner{Opener: prepared, Options: options}).RunObserved(ctx, prepared.Body, progress.apply)

	result.RequestDigest = prepared.RequestDigest
	result.Attempted = result.Attempted || prepared.Submitted
	result.Acknowledged = report.Acknowledged
	result.Complete = report.ProtocolComplete
	result.Events = report.Events
	result.Attempts = report.Attempts
	result.AttemptRecords = append([]inference.Attempt(nil), prepared.Attempts...)
	if err != nil {
		return result, report.ConversationID, err
	}
	if err = prepared.VerifyIdentity(ctx); err != nil {
		return result, "", err
	}
	if err = p.verifyAccount(ctx, a.ID, s.Revision); err != nil {
		return result, "", err
	}
	if err = prepared.Close(); err != nil {
		return result, "", runtimeError("desktop_cleanup_uncertain")
	}
	result.Text = progress.text
	if !result.Complete || !result.Acknowledged || strings.TrimSpace(result.Text) == "" {
		return result, "", stream.ErrIncomplete
	}
	return result, report.ConversationID, nil
}

func (p *TextProvider) verifyAccount(ctx context.Context, id, revision string) error {
	current, err := (VaultSessions{p.Vault}).Session(ctx, id)
	if err != nil || current.Revision != revision {
		return runtimeError("desktop_binding_invalid")
	}
	return nil
}
