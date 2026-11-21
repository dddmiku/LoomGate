package hotupdate

import (
	"os"
	"testing"
)

func TestSwitchJournalFailureCanBeClearedBySuccessfulExplicitCheck(t *testing.T) {
	m, _ := signedStagingManager(t, false)
	journal := switchJournal{Format: 1, State: StateFailed, Target: "v99.0.1", Error: "新版启动失败，已恢复上一版本"}
	if err := writeSwitchJournal(m.opts.Dir, journal); err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); got.State != StateFailed || got.LastError == "" {
		t.Fatal("restored process lost the previous failure")
	}
	if _, err := m.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); got.State != StateIdle || !got.UpdateReady || got.LastError != "" {
		t.Fatal("successful explicit check could not release a restored failed intent")
	}
}

func TestSwitchJournalCandidateAdvancesFromHandoverWhenParentCommits(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	journal := switchJournal{Format: 1, State: StateHandover, Target: "v99.0.2", Candidate: self}
	if err = writeSwitchJournal(dir, journal); err != nil {
		t.Fatal(err)
	}
	m := NewManager(Options{Dir: dir})
	if m.Status().State != StateHandover {
		t.Fatal("candidate claimed success before parent committed")
	}
	journal.State = StateIdle
	if err = writeSwitchJournal(dir, journal); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != StateIdle {
		t.Fatal("new process remained stuck in handover after commit")
	}
}
