package hotupdate

import "testing"

// The 1.x product line stays on the existing LoomGate repository. The legacy
// maintenance repository remains available only as a historical rollback
// source; silently pointing new builds back at it would split update channels.
func TestDefaultRepoIsTheLoomGateRepository(t *testing.T) {
	if DefaultRepo != "dddmiku/LoomGate" {
		t.Fatalf("DefaultRepo = %q, want dddmiku/LoomGate", DefaultRepo)
	}
}
