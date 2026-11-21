package hotupdate

import (
	"os"
	"path"
	"runtime"
	"strings"
)

const SupervisorCapabilityEnv = "LOOMGATE_UPDATE_SUPERVISOR"
const adoptingSupervisorCapability = "adopt-v1"

// AdoptingSupervisor only promises automatic sequential updates when the
// launch chain can retain/adopt an ExitHandover successor. Recognizing the
// established PID1 script also upgrades existing container images without
// requiring a docker socket or changing their saved configuration. The
// capability is inherited by later successors, whose temporary parent is the
// old gateway rather than PID1. Desktop/Android launchers do not advertise it.
func AdoptingSupervisor() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv(SupervisorCapabilityEnv) == adoptingSupervisorCapability {
		return true
	}
	if os.Getppid() != 1 {
		return false
	}
	raw, err := os.ReadFile("/proc/1/cmdline")
	if err != nil || !isAdoptingEntrypoint(string(raw)) {
		return false
	}
	return os.Setenv(SupervisorCapabilityEnv, adoptingSupervisorCapability) == nil
}

func isAdoptingEntrypoint(command string) bool {
	for _, argument := range strings.Split(command, "\x00") {
		if argument != "" && path.Clean(argument) == "/app/docker-entrypoint.sh" {
			return true
		}
	}
	return false
}
