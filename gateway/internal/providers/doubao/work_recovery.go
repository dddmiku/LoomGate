package doubao

import (
	"context"
	"encoding/json"
)

type workCursor struct {
	Session string `json:"session"`
	Message string `json:"message"`
}

// TaskWorkComplete reconciles an existing task journal. It has no public route
// and never lists artifacts, downloads files, or starts new work.
func (p *TextProvider) TaskWorkComplete(ctx context.Context, accountID, ref string) (bool, error) {
	if p.Dialogues == nil {
		return false, dialogueError("provider_conversation_unavailable")
	}
	account, session, err := p.account(ctx)
	if err != nil || accountID == "" || account.ID != accountID {
		return false, runtimeError("desktop_identity_mismatch")
	}
	meta, raw, err := p.Dialogues.Vault.Get(ref)
	if err != nil {
		return false, err
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	var record dialogueRecord
	if json.Unmarshal(raw, &record) != nil || meta.Provider != "doubao-dialogue" || record.Account != account.ID || record.AccountRevision != session.Revision {
		return false, runtimeError("desktop_identity_mismatch")
	}
	var cursor workCursor
	if len(record.Recovery) > 0 {
		if json.Unmarshal(record.Recovery, &cursor) != nil {
			return false, dialogueError("provider_recovery_proof_missing")
		}
	} else if record.Turn != 0 || record.State != "uncertain" {
		return false, dialogueError("provider_recovery_proof_missing")
	}
	if !runtimeBotID.MatchString(record.Conversation) || cursor.Session != "" && !runtimeBotID.MatchString(cursor.Session) || cursor.Message != "" && !runtimeBotID.MatchString(cursor.Message) {
		return false, runtimeError("desktop_binding_invalid")
	}
	reply, err := p.Runtime.run(ctx, runtimeRequest{Operation: "work_read", Identity: session.Runtime.Identity, Conversation: record.Conversation, Session: cursor.Session, Message: cursor.Message})
	if err != nil {
		return false, err
	}
	var data struct {
		Complete *bool `json:"complete"`
	}
	if reply.Identity != session.Runtime.Identity || json.Unmarshal(reply.Data, &data) != nil || data.Complete == nil {
		return false, runtimeError("desktop_protocol_invalid")
	}
	if err = p.verifyAccount(ctx, account.ID, session.Revision); err != nil {
		return false, err
	}
	return *data.Complete, nil
}
