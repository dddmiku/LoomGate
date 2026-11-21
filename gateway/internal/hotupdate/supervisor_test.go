package hotupdate

import "testing"

func TestAdoptingSupervisorRecognizesOnlyEstablishedPID1Entrypoint(t *testing.T) {
	for command, want := range map[string]bool{
		"/bin/sh\x00/app/docker-entrypoint.sh\x00-config\x00/app/config/config.json\x00": true,
		"/app/wb2api\x00-config\x00/app/config/config.json\x00":                          false,
		"/usr/bin/desktop-launcher\x00":                                                  false,
		"/bin/sh\x00-c\x00echo /app/docker-entrypoint.sh\x00":                            false,
	} {
		if got := isAdoptingEntrypoint(command); got != want {
			t.Fatalf("supervisor identity=%v want %v", got, want)
		}
	}
}
