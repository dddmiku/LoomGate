package doubao

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao/stream"
)

// HTTPTextProvider serves Doubao models with the captured official request
// template only. It does not start, attach to, or require the desktop client.
// This mirrors how the WorkBuddy and Trae providers work: a stored session
// cookie plus a protocol template, reverse-proxied behind the gateway.
type HTTPTextProvider struct {
	Vault  *accountvault.Vault
	Client *Client

	mu     queryMu
	cached *catalogCache2
}

type queryMu struct{}

type catalogCache2 struct {
	models  []inference.Model
	expires time.Time
	rev     string
}

func (p *HTTPTextProvider) session(ctx context.Context) (accountvault.Account, Session, error) {
	rows, err := p.Vault.List()
	if err != nil {
		return accountvault.Account{}, Session{}, runtimeError("desktop_binding_invalid")
	}
	var firstErr error
	for _, a := range rows {
		if a.Provider != "doubao" || !a.Enabled {
			continue
		}
		s, err := (VaultSessions{p.Vault}).Session(ctx, a.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return a, s, nil
	}
	if firstErr != nil {
		return accountvault.Account{}, Session{}, firstErr
	}
	return accountvault.Account{}, Session{}, runtimeError("login_required")
}

func (p *HTTPTextProvider) Models(ctx context.Context) ([]inference.Model, error) {
	a, _, err := p.session(ctx)
	if err != nil {
		return nil, err
	}
	c, err := p.Client.Catalog(ctx, a.ID, false)
	if err != nil {
		return nil, err
	}
	models := make([]inference.Model, 0, len(c.Models))
	for _, m := range c.Models {
		models = append(models, inference.Model{
			ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow,
			// The captured template is a single-turn completion. Multi-turn is
			// only advertised when the caller uses the conversation store.
			Incremental: true, MultiTurn: false,
		})
	}
	return models, nil
}

func (p *HTTPTextProvider) Complete(ctx context.Context, model, text string) (inference.Result, error) {
	return p.complete(ctx, model, text, "", "", "", nil, "")
}

func (p *HTTPTextProvider) Stream(ctx context.Context, model, text string, emit func(string) error) (inference.Result, error) {
	if emit == nil {
		return inference.Result{}, inference.ErrStreamContract
	}
	return p.complete(ctx, model, text, "", "", "", emit, "")
}

// CompleteDialogue implements multi-turn by sending the full transcript each
// turn through the captured template (stateless server-side conversation).
func (p *HTTPTextProvider) CompleteDialogue(ctx context.Context, q inference.Dialogue, emit func(string) error) (inference.Result, error) {
	if err := validateDialogue(q); err != nil {
		return inference.Result{}, err
	}
	prompt := flattenDialogue(q.Messages)
	return p.complete(ctx, q.Model, prompt, "", q.AccountID, q.AccountRevision, emit, "")
}

// StreamWithEffort is the effort-aware entry point. Callers that pass an empty
// effort keep whatever the captured template contained.
func (p *HTTPTextProvider) StreamWithEffort(ctx context.Context, model, text, effort string, emit func(string) error) (inference.Result, error) {
	if emit == nil {
		return inference.Result{}, inference.ErrStreamContract
	}
	return p.complete(ctx, model, text, "", "", "", emit, effort)
}

// flattenDialogue folds history into the single prompt slot the captured
// template carries. It is explicit about roles so the model can distinguish
// turns without inventing a server-side conversation id.
func flattenDialogue(messages []inference.Message) string {
	if len(messages) == 1 {
		return messages[0].Content
	}
	var b strings.Builder
	for _, m := range messages {
		if m.Role == "assistant" {
			b.WriteString("Assistant: ")
		} else {
			b.WriteString("User: ")
		}
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}
	b.WriteString("Assistant: ")
	return b.String()
}

func (p *HTTPTextProvider) complete(ctx context.Context, model, text, conversation, expectedAccount, expectedRevision string, emit func(string) error, effort string) (inference.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	a, s, err := p.session(ctx)
	if err != nil {
		return inference.Result{}, err
	}
	if expectedAccount != "" && (a.ID != expectedAccount || s.Revision != expectedRevision) {
		return inference.Result{}, runtimeError("desktop_identity_mismatch")
	}
	if len(s.Completion) == 0 {
		return inference.Result{}, runtimeError("desktop_operation_unsupported")
	}

	result := inference.Result{AccountRef: fmt.Sprintf("doubao-%x", sha256.Sum256([]byte(a.ID)))[:19]}

	// Resolve the reasoning effort against this model's published ladder. An
	// unrecognised token falls back to the template rather than silently
	// downgrading the request. A recognised level the model does not publish is
	// DOWN-GRADED to the model's highest level at or below it - never rejected.
	level, withEffort := 0, false
	if effort != "" {
		if parsed, ok := ParseEffort(effort); ok {
			level, withEffort = parsed, true
			if allowed := p.modelEfforts(ctx, a.ID, model); len(allowed) > 0 {
				clamped := ClampEffort(level, allowed)
				if clamped != level {
					logEffortClamp(model, level, clamped, allowed)
				}
				level = clamped
			}
		}
	}

	body, err := buildCompletionBodyFor(s.Completion, s.BotID, model, text, conversation, level, withEffort)
	if err != nil {
		return result, err
	}
	session := &HTTPTextSession{session: s, model: model, prompt: text, conversation: conversation, Body: body}
	defer session.Close()

	progress := textProgress{}
	if emit != nil {
		progress.emit = emit
	}
	report, runErr := (stream.Runner{
		Opener:  session,
		Options: stream.Options{Timeout: 4 * time.Minute, AttemptTimeout: 120 * time.Second, MaxResumes: 1},
	}).RunObserved(ctx, body, progress.apply)

	result.RequestDigest = ""
	result.MessageID = report.MessageID
	result.Attempted = session.Submitted()
	result.Acknowledged = report.Acknowledged
	result.Complete = report.ProtocolComplete
	result.Events = report.Events
	result.Attempts = report.Attempts
	result.AttemptRecords = append([]inference.Attempt(nil), session.Attempts...)
	if runErr != nil {
		return result, runErr
	}
	result.Text = progress.text
	if !result.Complete || !result.Acknowledged || strings.TrimSpace(result.Text) == "" {
		return result, stream.ErrIncomplete
	}
	return result, nil
}

var _ = json.Marshal

// modelEfforts returns the published reasoning levels for one model, or nil when
// the catalog is unavailable. A nil result means "do not clamp".
func (p *HTTPTextProvider) modelEfforts(ctx context.Context, account, model string) []int {
	catalog, err := p.Client.Catalog(ctx, account, false)
	if err != nil {
		return nil
	}
	for _, m := range catalog.Models {
		if m.ID != model {
			continue
		}
		out := make([]int, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			out = append(out, e.Level)
		}
		return out
	}
	return nil
}

// effortClampOnce keeps the adjustment notice to one line per model/level pair,
// matching the legacy upstream path so logs stay readable.
var effortClampOnce sync.Map

func logEffortClamp(model string, from, to int, allowed []int) {
	key := fmt.Sprintf("%s\x00%d\x00%d", model, from, to)
	if _, loaded := effortClampOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Printf("WARN: [豆包] 推理档位 downgraded 模型=%s %d -> %d（该模型可选 %v；同一组合只记一次）",
		model, from, to, allowed)
}
