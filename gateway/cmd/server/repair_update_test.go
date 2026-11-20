package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepairCommandRequiresAnUnambiguousExistingDeploymentDirectory(t *testing.T) {
	t.Setenv("WB2API_UPDATE_DIR", "")
	t.Setenv("WB2A_STATE_FILE", "")
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte(`{"state_file":"relative/state.json"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runRepairUpdate(config, "status", "", "", &output); err == nil {
		t.Fatal("ambiguous relative deployment directory was accepted")
	}
	if output.Len() != 0 {
		t.Fatal("rejected recovery emitted a success response")
	}
	if err := runRepairUpdate(config, "status", "", dir, &output); err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if json.Unmarshal(output.Bytes(), &status) != nil || status["state"] != "not_prepared" {
		t.Fatal("status did not inspect the explicit update directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "relative")); !os.IsNotExist(err) {
		t.Fatal("status created a second deployment from its working directory")
	}
}

func TestRepairCommandHelperProcess(t *testing.T) {
	if os.Getenv("LOOMGATE_TEST_REPAIR_CLI") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("repair-cli", flag.ExitOnError)
	os.Args = []string{os.Args[0], "-config", os.Getenv("LOOMGATE_TEST_REPAIR_CONFIG"),
		"-repair-update-status", "-repair-update-dir", os.Getenv("LOOMGATE_TEST_REPAIR_DIR")}
	if os.Getenv("LOOMGATE_TEST_REPAIR_MULTI") == "1" {
		os.Args = append(os.Args, "-repair-update")
	}
	main()
	os.Exit(0)
}

func TestRepairCLIRejectsMultipleActionsBeforeReadingConfiguration(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestRepairCommandHelperProcess$")
	command.Env = append(os.Environ(), "LOOMGATE_TEST_REPAIR_CLI=1", "LOOMGATE_TEST_REPAIR_MULTI=1")
	output, err := command.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("repair_action_invalid")) {
		t.Fatal("multiple recovery actions were accepted")
	}
	if bytes.Contains(output, []byte("load config")) {
		t.Fatal("invalid recovery action opened configuration")
	}
}

func TestRepairCLIStatusReturnsBeforeOpeningTaskEngineOrAccounts(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	data := map[string]any{"auth_dir": filepath.Join(dir, "must-not-open-auths"),
		"state_file": filepath.Join(dir, "must-not-open-state", "state.json")}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRepairCommandHelperProcess$")
	command.Env = append(os.Environ(), "LOOMGATE_TEST_REPAIR_CLI=1",
		"LOOMGATE_TEST_REPAIR_CONFIG="+config, "LOOMGATE_TEST_REPAIR_DIR="+dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery CLI failed: %v", err)
	}
	var status map[string]any
	if json.Unmarshal(output, &status) != nil || status["state"] != "not_prepared" {
		t.Fatal("CLI emitted runtime logs or did not return its allowlisted status")
	}
	for _, name := range []string{"must-not-open-auths", "must-not-open-state", "task-engine.json", "provider-accounts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("recovery opened application state")
		}
	}
}

func TestRepairCommandDoesNotCreateMissingConfiguration(t *testing.T) {
	config := filepath.Join(t.TempDir(), "absent.json")
	if err := runRepairUpdate(config, "stage", "", t.TempDir(), io.Discard); err == nil {
		t.Fatal("recovery invented a new deployment configuration")
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatal("recovery wrote a missing configuration")
	}
}
