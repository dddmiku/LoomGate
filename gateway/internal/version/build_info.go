package version

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
)

// The native packaging check exits before main can read accounts or start a
// service. Only public build metadata and the verifier key digest are returned.
func init() {
	if len(os.Args) != 2 || os.Args[1] != "--loomgate-build-info" {
		return
	}
	public, err := hex.DecodeString(ReleaseKey)
	valid := err == nil && len(public) == 32
	fingerprint := ""
	if valid {
		sum := sha256.Sum256(public)
		fingerprint = hex.EncodeToString(sum[:])
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"version": Version, "commit": Commit, "goos": runtime.GOOS, "goarch": runtime.GOARCH, "verification_enabled": valid, "release_key_sha256": fingerprint})
	if !valid {
		os.Exit(1)
	}
	os.Exit(0)
}
