package doubao

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

// DialogueStore persists only transcript digests and encrypted official cursors.
// A pending turn surviving process death remains uncertain; it is never replayed.
type DialogueStore struct {
	Vault *accountvault.Vault
	mu    sync.Mutex
}
type dialogueRecord struct {
	Version         int             `json:"version"`
	Owner           string          `json:"owner"`
	Account         string          `json:"account"`
	AccountRevision string          `json:"account_revision"`
	Model           string          `json:"model"`
	Transcript      string          `json:"transcript"`
	Conversation    string          `json:"conversation"`
	State           string          `json:"state"`
	Turn            int             `json:"turn"`
	Updated         time.Time       `json:"updated"`
	LegacyWork      json.RawMessage `json:"work,omitempty"`
	Recovery        json.RawMessage `json:"recovery,omitempty"`
	RequestKey      string          `json:"request_key,omitempty"`
	RequestDigest   string          `json:"request_digest,omitempty"`
}
type dialogueTurn struct {
	ID       string
	Revision uint64
	Before   dialogueRecord
	Record   dialogueRecord
}

func (*dialogueTurn) String() string   { return "doubao.dialogueTurn[redacted]" }
func (*dialogueTurn) GoString() string { return "doubao.dialogueTurn[redacted]" }

type dialogueError string

func (e dialogueError) Error() string      { return string(e) }
func (e dialogueError) PublicCode() string { return string(e) }
func transcript(messages []inference.Message) string {
	b, _ := json.Marshal(messages)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func validateDialogue(q inference.Dialogue) error {
	if len(q.IdempotencyKey) > 128 || strings.ContainsAny(q.IdempotencyKey, "\r\n\x00") {
		return dialogueError("provider_idempotency_invalid")
	}
	if _, err := hex.DecodeString(q.Owner); err != nil {
		return dialogueError("provider_context_unsupported")
	}
	// Role order is not constrained: the transcript is flattened into one prompt,
	// so an assistant prefill, two consecutive user turns or an assistant-first
	// history are all representable. Only empty turns, unknown roles and sizes are
	// rejected.
	if len(q.Owner) != 64 || len(q.Messages) == 0 || len(q.Messages) > 101 {
		return dialogueError("provider_context_unsupported")
	}
	total := 0
	for _, m := range q.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return dialogueError("provider_context_unsupported")
		}
		total += len(m.Content)
		if strings.TrimSpace(m.Content) == "" || len(m.Content) > 65536 || total > 1<<20 {
			return dialogueError("provider_context_unsupported")
		}
	}
	if q.ConversationRef != "" && (len(q.ConversationRef) != 39 || !strings.HasPrefix(q.ConversationRef, "dialog-")) {
		return dialogueError("provider_conversation_invalid")
	}
	return nil
}
func (s *DialogueStore) begin(q inference.Dialogue, account, revision string) (*dialogueTurn, error) {
	if err := validateDialogue(q); err != nil {
		return nil, err
	}
	if s == nil || s.Vault == nil {
		return nil, dialogueError("provider_conversation_unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.Vault.List()
	if err != nil {
		return nil, err
	}
	prefix := ""
	if len(q.Messages) > 1 {
		prefix = transcript(q.Messages[:len(q.Messages)-1])
	}
	var found *dialogueTurn
	requestKey, requestDigest, initialID := "", "", ""
	if q.IdempotencyKey != "" {
		key := sha256.Sum256([]byte(q.Owner + "\x00" + account + "\x00" + q.IdempotencyKey))
		requestKey = hex.EncodeToString(key[:])
		digest := sha256.Sum256([]byte(q.Model + "\x00" + transcript(q.Messages)))
		requestDigest = hex.EncodeToString(digest[:])
		if prefix == "" && q.ConversationRef == "" {
			initialID = "dialog-" + requestKey[:32]
		}
	}
	count := 0
	for _, a := range rows {
		if a.Provider != "doubao-dialogue" {
			continue
		}
		count++
		if q.ConversationRef != "" && a.ID != q.ConversationRef {
			continue
		}
		if q.ConversationRef == "" && prefix == "" && a.ID != initialID {
			continue
		}
		_, raw, e := s.Vault.Get(a.ID)
		if e != nil {
			return nil, e
		}
		var record dialogueRecord
		e = json.Unmarshal(raw, &record)
		for i := range raw {
			raw[i] = 0
		}
		if e != nil || record.Version != 1 {
			return nil, errors.New("dialogue_store_invalid")
		}
		if record.Owner != q.Owner || record.Account != account {
			continue
		}
		if requestKey != "" && record.RequestKey == requestKey {
			if record.RequestDigest != requestDigest {
				return nil, dialogueError("provider_idempotency_conflict")
			}
			if record.State != "ready" {
				return nil, dialogueError("provider_turn_uncertain")
			}
			return nil, dialogueError("provider_request_already_processed")
		}
		if record.Model != q.Model {
			continue
		}
		if record.Transcript != prefix {
			continue
		}
		if found != nil {
			return nil, dialogueError("provider_conversation_ambiguous")
		}
		found = &dialogueTurn{ID: a.ID, Revision: a.Revision, Before: record, Record: record}
	}
	if found == nil {
		if prefix != "" || q.ConversationRef != "" {
			return nil, dialogueError("provider_context_mismatch")
		}
		if count >= 2048 {
			return nil, dialogueError("provider_conversation_capacity")
		}
		var id [16]byte
		if _, err = rand.Read(id[:]); err != nil {
			return nil, err
		}
		found = &dialogueTurn{ID: "dialog-" + hex.EncodeToString(id[:]), Record: dialogueRecord{Version: 1, Owner: q.Owner, Account: account, AccountRevision: revision, Model: q.Model, State: "ready"}}
		if initialID != "" {
			found.ID = initialID
		}
	}
	if found.Record.AccountRevision != revision {
		return nil, dialogueError("provider_conversation_account_changed")
	}
	if found.Record.State != "ready" {
		return nil, dialogueError("provider_turn_uncertain")
	}
	if found.Record.Turn >= 50 {
		return nil, dialogueError("provider_conversation_limit")
	}
	found.Record.State = "pending"
	found.Record.RequestKey = requestKey
	found.Record.RequestDigest = requestDigest
	found.Record.Updated = time.Now()
	b, _ := json.Marshal(found.Record)
	stored, err := s.Vault.Put(accountvault.Account{ID: found.ID, Provider: "doubao-dialogue", Enabled: true}, b, found.Revision)
	if err != nil {
		return nil, err
	}
	found.Revision = stored.Revision
	return found, nil
}
func (s *DialogueStore) finish(t *dialogueTurn, q inference.Dialogue, result inference.Result, conversation string, completed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := t.Record
	next.State = "uncertain"
	if len(result.Recovery) > 0 {
		next.Recovery = append(json.RawMessage(nil), result.Recovery...)
	}
	if conversation != "" && len(conversation) <= 128 && !strings.ContainsAny(conversation, "\r\n\x00") {
		next.Conversation = conversation
	}
	if completed {
		if conversation == "" || len(conversation) > 128 || strings.ContainsAny(conversation, "\r\n\x00") {
			return dialogueError("provider_conversation_invalid")
		}
		next.State = "ready"
		next.Conversation = conversation
		next.Turn++
		next.Transcript = transcript(append(append([]inference.Message{}, q.Messages...), inference.Message{Role: "assistant", Content: result.Text}))
	} else if !result.Attempted && t.Before.Version == 1 {
		next = t.Before
	} else if !result.Attempted {
		next.State = "ready"
		next.RequestKey = ""
		next.RequestDigest = ""
	}
	next.Updated = time.Now()
	b, _ := json.Marshal(next)
	_, err := s.Vault.Put(accountvault.Account{ID: t.ID, Provider: "doubao-dialogue", Enabled: true}, b, t.Revision)
	return err
}
