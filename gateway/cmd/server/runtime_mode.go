package main

import "os"

// LoomGate now defaults to the new workbench/task engine and console. The old
// scheduler and legacy Python panel remain available only as an explicit
// compatibility mode for an operator who has not migrated yet; they must never
// start concurrently with the new engine.
//
// LOOMGATE_LEGACY_SCHEDULER=1 is the single opt-out switch. LOOMGATE_REBUILD
// is still accepted for older automation that explicitly asked for the new
// runtime, but it is no longer required.
func legacyRuntimeMode() bool {
	value := os.Getenv("LOOMGATE_LEGACY_SCHEDULER")
	return value == "1" || value == "true"
}

func useLoomGateWorkbench() bool {
	return !legacyRuntimeMode()
}
