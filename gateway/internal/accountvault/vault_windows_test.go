//go:build windows

package accountvault_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"workbuddy2api/internal/accountvault"
)

func hideWorker(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func checkPrivateACL(t *testing.T, path string) {
	t.Helper()
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		shell, err = exec.LookPath("powershell")
	}
	if err != nil {
		t.Fatal("PowerShell required for real Windows ACL check")
	}
	script := "$ErrorActionPreference='Stop'; $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value; " +
		"$allowed=@($sid,'S-1-3-4','S-1-5-18','S-1-5-32-544'); $acl=Get-Acl -LiteralPath '" + strings.ReplaceAll(path, "'", "''") +
		"'; $bad=@($acl.Access | Where-Object { $_.AccessControlType -eq 'Allow' -and $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value -notin $allowed }); " +
		"[pscustomobject]@{unsafe=$bad.Count; entries=$acl.Access.Count} | ConvertTo-Json -Compress"
	cmd := exec.Command(shell, "-NoProfile", "-Command", script)
	hideWorker(cmd)
	for _, value := range os.Environ() {
		if strings.EqualFold(filepath.Base(shell), "powershell.exe") && strings.HasPrefix(strings.ToLower(value), "psmodulepath=") {
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("Windows ACL query failed")
	}
	var result struct {
		Unsafe  int `json:"unsafe"`
		Entries int `json:"entries"`
	}
	if json.Unmarshal(out, &result) != nil || result.Entries == 0 {
		t.Fatal("Windows ACL query returned no usable result")
	}
	if result.Unsafe != 0 {
		t.Fatal("vault grants access outside its owner, administrators or SYSTEM")
	}
}

func TestVaultWindowsRealACLProtectsDirectoryAndFiles(t *testing.T) {
	v, dir := openFixture(t)
	a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-session"), 0)
	for _, path := range []string{dir, filepath.Join(dir, "master.key"), filepath.Join(dir, a.ID+".account")} {
		checkPrivateACL(t, path)
	}
}

func TestVaultWindowsExistingBroadACLIsRejectedOrRestricted(t *testing.T) {
	dir := filepath.Join(fixtureRoot(t), "vault")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal("cannot prepare broad-ACL synthetic directory")
	}
	cmd := exec.Command("icacls.exe", dir, "/grant", "*S-1-1-0:(OI)(CI)F", "/Q")
	hideWorker(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal("cannot prepare broad-ACL fixture")
	}
	v, err := accountvault.Open(dir)
	if err != nil {
		return // A private store may conservatively refuse unsafe existing storage.
	}
	t.Cleanup(func() { _ = v.Close() })
	checkPrivateACL(t, dir)
	a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-session"), 0)
	checkPrivateACL(t, filepath.Join(dir, a.ID+".account"))
}

func TestVaultWindowsRejectsJunctionAncestor(t *testing.T) {
	root := fixtureRoot(t)
	out := filepath.Join(root, "outside")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("cannot prepare isolated junction target")
	}
	junction := filepath.Join(root, "alias")
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell 7 unavailable for junction fixture")
	}
	script := "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path '" + strings.ReplaceAll(junction, "'", "''") +
		"' -Target '" + strings.ReplaceAll(out, "'", "''") + "' | Out-Null"
	cmd := exec.Command(shell, "-NoProfile", "-Command", script)
	hideWorker(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal("cannot create isolated junction")
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
	if _, err := accountvault.Open(filepath.Join(junction, "vault")); err == nil {
		t.Fatal("Open followed a junction ancestor")
	}
	if _, err := os.Stat(filepath.Join(out, "vault")); !os.IsNotExist(err) {
		t.Fatal("rejected junction caused writes outside the selected path")
	}
}
