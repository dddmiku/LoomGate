package main

import "testing"

func TestRuntimeModeDefaultsToLoomGateAndKeepsExplicitLegacyOptOut(t *testing.T) {
	t.Setenv("LOOMGATE_LEGACY_SCHEDULER", "")
	if !useLoomGateWorkbench() || legacyRuntimeMode() {
		t.Fatal("default must be the LoomGate workbench")
	}
	t.Setenv("LOOMGATE_LEGACY_SCHEDULER", "1")
	if useLoomGateWorkbench() || !legacyRuntimeMode() {
		t.Fatal("explicit legacy scheduler mode was ignored")
	}
}
