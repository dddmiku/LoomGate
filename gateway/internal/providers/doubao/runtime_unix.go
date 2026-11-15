//go:build !windows

package doubao

import "os/exec"

func configureDesktopProcess(cmd *exec.Cmd) {}
