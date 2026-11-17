package doubao

import (
	"path/filepath"
	"strings"
	"testing"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

func dialogueFixture(t *testing.T) (*DialogueStore, inference.Dialogue) {
	t.Helper()
	v, err := accountvault.Open(filepath.Join(t.TempDir(), "dialogues"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return &DialogueStore{Vault: v}, inference.Dialogue{Owner: strings.Repeat("a", 64), Model: "doubao:synthetic:ordinary", Messages: []inference.Message{{Role: "user", Content: "Remember synthetic value 17"}}}
}
func TestDialogueMaintainsContextAndRejectsCrossOwnerOrChangedHistory(t *testing.T) {
	s, q := dialogueFixture(t)
	first, err := s.begin(q, "synthetic-account", "revision1")
	if err != nil {
		t.Fatal(err)
	}
	result := inference.Result{Text: "Synthetic acknowledgement", Attempted: true, Acknowledged: true, Complete: true}
	if err = s.finish(first, q, result, "synthetic-official-conversation", true); err != nil {
		t.Fatal(err)
	}
	follow := q
	follow.Messages = append(append([]inference.Message{}, q.Messages...), inference.Message{Role: "assistant", Content: result.Text}, inference.Message{Role: "user", Content: "Add two to the remembered value"})
	for _, kind := range []string{"owner", "account", "revision", "model", "history"} {
		bad := follow
		bad.Messages = append([]inference.Message{}, follow.Messages...)
		account, revision := "synthetic-account", "revision1"
		switch kind {
		case "owner":
			bad.Owner = strings.Repeat("b", 64)
		case "account":
			account = "another-account"
		case "revision":
			revision = "revision2"
		case "model":
			bad.Model += "-changed"
		case "history":
			bad.Messages[1].Content = "unverified assistant response"
		}
		if _, err = s.begin(bad, account, revision); err == nil {
			t.Fatalf("%s context accepted", kind)
		}
	}
	next, err := s.begin(follow, "synthetic-account", "revision1")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != first.ID || next.Record.Conversation != "synthetic-official-conversation" || next.Record.Turn != 1 {
		t.Fatal("official conversation lost")
	}
	if _, err = (&DialogueStore{Vault: s.Vault}).begin(follow, "synthetic-account", "revision1"); err == nil {
		t.Fatal("pending turn replayed by restarted provider")
	}
	if err = s.finish(next, follow, inference.Result{Attempted: true}, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.begin(follow, "synthetic-account", "revision1"); err == nil {
		t.Fatal("unknown turn replayed")
	}
}
func TestDialogueNoSubmissionCanResumeButStaleCompletionCannotOverwrite(t *testing.T) {
	s, q := dialogueFixture(t)
	first, _ := s.begin(q, "account", "r1")
	done := inference.Result{Text: "synthetic answer", Attempted: true, Complete: true, Acknowledged: true}
	if err := s.finish(first, q, done, "synthetic-conversation", true); err != nil {
		t.Fatal(err)
	}
	q.Messages = append(q.Messages, inference.Message{Role: "assistant", Content: done.Text}, inference.Message{Role: "user", Content: "followup"})
	turn, err := s.begin(q, "account", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finish(turn, q, inference.Result{}, "", false); err != nil {
		t.Fatal(err)
	}
	next, err := s.begin(q, "account", "r1")
	if err != nil {
		t.Fatal("preflight-only failure remained uncertain", err)
	}
	if err = s.finish(turn, q, done, "wrong-conversation", true); err == nil {
		t.Fatal("stale completion overwrote newer turn")
	}
	if err = s.finish(next, q, done, "synthetic-conversation", true); err != nil {
		t.Fatal(err)
	}
}
func TestDialogueRejectsInventedToolRolesAndMalformedHistory(t *testing.T) {
	_, q := dialogueFixture(t)
	for _, messages := range [][]inference.Message{nil, {{Role: "system", Content: "x"}}, {{Role: "tool", Content: "invented"}}, {{Role: "user", Content: "x"}, {Role: "assistant", Content: "y"}}, {{Role: "user", Content: "x"}, {Role: "user", Content: "y"}, {Role: "user", Content: "z"}}} {
		q.Messages = messages
		if validateDialogue(q) == nil {
			t.Fatal("unsupported context accepted")
		}
	}
}

func TestDialogueFirstRequestIdempotencySurvivesUncertainAndCompletedRestart(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "completed"}[complete], func(t *testing.T) {
			s, q := dialogueFixture(t)
			q.IdempotencyKey = "synthetic-request"
			turn, err := s.begin(q, "account", "r1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = (&DialogueStore{Vault: s.Vault}).begin(q, "account", "r1"); err == nil {
				t.Fatal("pending first request replayed")
			}
			if err = s.finish(turn, q, inference.Result{Attempted: true, Complete: complete, Text: "synthetic answer"}, "synthetic-conversation", complete); err != nil {
				t.Fatal(err)
			}
			if _, err = (&DialogueStore{Vault: s.Vault}).begin(q, "account", "r1"); err == nil {
				t.Fatal("finished or uncertain first request replayed")
			}
			q.Messages = []inference.Message{{Role: "user", Content: "changed operation"}}
			if _, err = s.begin(q, "account", "r1"); err != dialogueError("provider_idempotency_conflict") {
				t.Fatal("same key accepted different content")
			}
		})
	}
}
func TestDialogueIdempotencyAllowsOnlyUnsubmittedRetry(t *testing.T) {
	s, q := dialogueFixture(t)
	q.IdempotencyKey = "synthetic-preflight"
	turn, err := s.begin(q, "account", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finish(turn, q, inference.Result{}, "", false); err != nil {
		t.Fatal(err)
	}
	next, err := s.begin(q, "account", "r1")
	if err != nil || next.ID != turn.ID {
		t.Fatal("proven unsubmitted retry could not reuse journal")
	}
}
