package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveManagedPythonReturnsAbsoluteInterpreter(t *testing.T) {
	for _, candidate := range []string{"python", "python3", "py"} {
		if _, err := exec.LookPath(candidate); err != nil {
			continue
		}
		resolved := resolveManagedPython(candidate)
		if resolved == "" || !filepath.IsAbs(resolved) {
			t.Fatalf("bare %q was not resolved to an absolute path: %q", candidate, resolved)
		}
		return
	}
	t.Skip("no python launcher on this host")
}

func TestResolveManagedPythonRejectsUnknownCommand(t *testing.T) {
	if got := resolveManagedPython("definitely-not-a-python-name"); got != "" {
		t.Fatalf("unknown interpreter accepted: %q", got)
	}
}
