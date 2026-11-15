//go:build !windows

package accountvault_test

import "os/exec"

func hideWorker(*exec.Cmd) {}
