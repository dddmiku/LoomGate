package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"workbuddy2api/internal/accountvault"
)

func TestTaskRecoveryHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "--task-recovery" {
		return
	}
	var in runtimeRequest
	if json.NewDecoder(os.Stdin).Decode(&in) != nil {
		os.Exit(2)
	}
	if in.Operation == "identity" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "identity": strings.Repeat("a", 64)})
	} else if in.Operation == "work_read" && in.Conversation == "synthetic-conversation" && in.Session == "synthetic-turn" && in.Message == "synthetic-message" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "identity": strings.Repeat("a", 64), "data": map[string]bool{"complete": true}})
	} else {
		fmt.Fprint(os.Stdout, `{"ok":false,"error":"desktop_ownership_lost"}`)
	}
	os.Exit(0)
}
func TestTaskRecoveryKeepsUnknownJournalAndRejectsWrongAccountRevisionTurn(t *testing.T) {
	root := t.TempDir()
	accounts, e := accountvault.Open(filepath.Join(root, "accounts"))
	if e != nil {
		t.Fatal(e)
	}
	defer accounts.Close()
	records, e := accountvault.Open(filepath.Join(root, "records"))
	if e != nil {
		t.Fatal(e)
	}
	defer records.Close()
	raw, _ := json.Marshal(StoredProfile{Runtime: &RuntimeBinding{Identity: strings.Repeat("a", 64)}, BotID: "synthetic-bot"})
	accounts.Put(accountvault.Account{ID: "synthetic", Provider: "doubao", Enabled: true}, raw, 0)
	exe, _ := os.Executable()
	runtime, e := NewCommandRuntime([]string{exe, "-test.run=^TestTaskRecoveryHelper$", "--", "--task-recovery"}, 19226)
	if e != nil {
		t.Fatal(e)
	}
	p := &TextProvider{Vault: accounts, Runtime: runtime, Dialogues: &DialogueStore{Vault: records}}
	ref := "dialog-" + strings.Repeat("a", 32)
	record := dialogueRecord{Version: 1, Account: "synthetic", AccountRevision: "1", Conversation: "synthetic-conversation", State: "uncertain", Recovery: json.RawMessage(`{"session":"synthetic-turn","message":"synthetic-message"}`), LegacyWork: json.RawMessage(`[{"kind":"historical"}]`)}
	put := func() {
		meta, _, _ := records.Get(ref)
		raw, _ = json.Marshal(record)
		if _, e = records.Put(accountvault.Account{ID: ref, Provider: "doubao-dialogue", Enabled: true}, raw, meta.Revision); e != nil {
			t.Fatal(e)
		}
	}
	put()
	before, original, _ := records.Get(ref)
	done, e := p.TaskWorkComplete(context.Background(), "synthetic", ref)
	if e != nil || !done {
		t.Fatal("readback failed", e)
	}
	after, current, _ := records.Get(ref)
	if before.Revision != after.Revision || string(original) != string(current) {
		t.Fatal("read-only recovery changed unknown record")
	}
	if _, e = p.TaskWorkComplete(context.Background(), "foreign", ref); e == nil {
		t.Fatal("foreign account accepted")
	}
	record.Recovery = json.RawMessage(`{"session":"foreign-turn","message":"synthetic-message"}`)
	put()
	if _, e = p.TaskWorkComplete(context.Background(), "synthetic", ref); e == nil {
		t.Fatal("foreign turn accepted")
	}
	record.AccountRevision = "2"
	put()
	if _, e = p.TaskWorkComplete(context.Background(), "synthetic", ref); e == nil {
		t.Fatal("old revision accepted")
	}
}
