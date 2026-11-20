package accountvault_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
)

// Every test creates synthetic records below the task's D-drive work directory.
// No test locates an installed profile, credentials file or production vault.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("LOOMGATE_WORK_ROOT")
	if base == "" {
		base = "D:/LoomGate-work"
	}
	base, err := filepath.Abs(filepath.Join(base, "accountvault-tests"))
	if err != nil || os.MkdirAll(base, 0700) != nil {
		t.Fatal("cannot create isolated fixture base")
	}
	root, err := os.MkdirTemp(base, "fixture-")
	if err != nil {
		t.Fatal("cannot create isolated fixture")
	}
	if err := os.WriteFile(filepath.Join(root, ".synthetic-test-case"), []byte("synthetic"), 0600); err != nil {
		t.Fatal("cannot mark isolated fixture")
	}
	t.Cleanup(func() {
		rel, err := filepath.Rel(base, root)
		if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			t.Error("refusing cleanup outside isolated fixture base")
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error("isolated fixture cleanup failed")
		}
	})
	return root
}

func openFixture(t *testing.T) (*accountvault.Vault, string) {
	t.Helper()
	dir := filepath.Join(fixtureRoot(t), "vault")
	v, err := accountvault.Open(dir)
	if err != nil {
		t.Fatalf("Open synthetic vault: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, dir
}

func fixtureAccount(id string) accountvault.Account {
	return accountvault.Account{ID: id, Provider: "synthetic-provider", Enabled: true}
}

func putFixture(t *testing.T, v *accountvault.Vault, a accountvault.Account, secret []byte, expected uint64) accountvault.Account {
	t.Helper()
	stored, err := v.Put(a, secret, expected)
	if err != nil {
		t.Fatalf("Put synthetic record: %v", err)
	}
	return stored
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read synthetic fixture")
	}
	return b
}

func writeFixture(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal("cannot write synthetic fixture")
	}
}

func envelope(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if json.Unmarshal(readFixture(t, path), &doc) != nil {
		t.Fatal("invalid synthetic envelope")
	}
	return doc
}

func writeEnvelope(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal("cannot encode synthetic envelope")
	}
	writeFixture(t, path, b)
}

func TestOpaqueIDIsStableAcrossReopenAndScopedToVault(t *testing.T) {
	v, dir := openFixture(t)
	before := v.OpaqueID("autoclaw", "synthetic-identity")
	if before == "" || strings.Contains(before, "synthetic-identity") {
		t.Fatal("opaque identity was empty or exposed its source")
	}
	if err := v.Close(); err != nil {
		t.Fatal("cannot close synthetic vault")
	}
	reopened, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal("cannot reopen synthetic vault")
	}
	defer reopened.Close()
	if got := reopened.OpaqueID("autoclaw", "synthetic-identity"); got != before {
		t.Fatal("opaque identity changed across vault reopen")
	}
	other, _ := openFixture(t)
	if other.OpaqueID("autoclaw", "synthetic-identity") == before {
		t.Fatal("independent vaults produced correlatable public identities")
	}
	if reopened.Close() != nil || reopened.OpaqueID("autoclaw", "synthetic-identity") != "" {
		t.Fatal("closed vault continued issuing identities")
	}
}

func TestVaultEncryptsSecretsAndReopensWithoutDisclosure(t *testing.T) {
	v, dir := openFixture(t)
	secret := []byte("synthetic-session-private-material-for-at-rest-test")
	first := putFixture(t, v, fixtureAccount("fixture-account"), secret, 0)
	if first.Revision != 1 || first.UpdatedAt.IsZero() || !first.Enabled {
		t.Fatal("created account metadata is incomplete")
	}
	path := filepath.Join(dir, first.ID+".account")
	before := readFixture(t, path)
	if bytes.Contains(before, secret) || bytes.Contains(before, []byte(base64.StdEncoding.EncodeToString(secret))) {
		t.Fatal("account file contains plaintext or merely encoded secret")
	}
	listed, err := v.List()
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatal("List did not return the stored descriptor")
	}
	public, err := json.Marshal(listed)
	if err != nil || bytes.Contains(public, secret) || bytes.Contains(public, []byte("ciphertext")) || bytes.Contains(public, []byte("nonce")) {
		t.Fatal("List exposed private storage material")
	}
	second := putFixture(t, v, first, secret, first.Revision)
	if second.Revision != 2 || bytes.Equal(before, readFixture(t, path)) {
		t.Fatal("update failed to advance or reseal the record")
	}
	if v.Close() != nil {
		t.Fatal("cannot release initial vault ownership")
	}
	reopened, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal("cannot reopen synthetic vault")
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, plain, err := reopened.Get(first.ID)
	if err != nil || got.Revision != 2 || !bytes.Equal(plain, secret) {
		t.Fatal("persisted encrypted session did not round-trip")
	}
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "owner.lock" {
			return nil // Windows mandatory locking makes this control file unreadable while owned.
		}
		if !entry.IsDir() && bytes.Contains(readFixture(t, path), secret) {
			t.Error("vault file contains unencrypted synthetic secret")
		}
		return nil
	})
	if err != nil {
		t.Fatal("cannot inspect synthetic vault files")
	}
}

func TestVaultAuthenticatesCiphertextAndAllAccountMetadata(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"ciphertext": func(doc map[string]any) {
			b, _ := base64.StdEncoding.DecodeString(doc["ciphertext"].(string))
			b[len(b)-1] ^= 1
			doc["ciphertext"] = base64.StdEncoding.EncodeToString(b)
		},
		"nonce":      func(doc map[string]any) { doc["nonce"] = "" },
		"provider":   func(doc map[string]any) { doc["account"].(map[string]any)["provider"] = "other-provider" },
		"identity":   func(doc map[string]any) { doc["account"].(map[string]any)["id"] = "other-account" },
		"revision":   func(doc map[string]any) { doc["account"].(map[string]any)["revision"] = float64(8) },
		"enabled":    func(doc map[string]any) { doc["account"].(map[string]any)["enabled"] = false },
		"updated_at": func(doc map[string]any) { doc["account"].(map[string]any)["updated_at"] = "2001-01-01T00:00:00Z" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			v, dir := openFixture(t)
			a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-tamper-session"), 0)
			path := filepath.Join(dir, a.ID+".account")
			doc := envelope(t, path)
			mutate(doc)
			writeEnvelope(t, path, doc)
			tampered := readFixture(t, path)
			if _, plain, err := v.Get(a.ID); err == nil || len(plain) != 0 {
				t.Error("Get accepted tampered metadata or ciphertext")
			}
			if rows, err := v.List(); err == nil || len(rows) != 0 {
				t.Error("List exposed unauthenticated account metadata")
			}
			if _, err := v.Put(a, []byte("synthetic-replacement"), a.Revision); err == nil {
				t.Error("Put silently replaced a corrupted account")
			}
			if !bytes.Equal(tampered, readFixture(t, path)) {
				t.Error("failed mutation changed corrupted storage")
			}
		})
	}
}

func TestVaultCiphertextCannotMoveBetweenIdentitiesOrProviders(t *testing.T) {
	v, dir := openFixture(t)
	a := putFixture(t, v, fixtureAccount("fixture-one"), []byte("synthetic-first-session"), 0)
	b := fixtureAccount("fixture-two")
	b.Provider = "another-provider"
	putFixture(t, v, b, []byte("synthetic-second-session"), 0)
	one := envelope(t, filepath.Join(dir, a.ID+".account"))
	twoPath := filepath.Join(dir, b.ID+".account")
	two := envelope(t, twoPath)
	two["nonce"], two["ciphertext"] = one["nonce"], one["ciphertext"]
	writeEnvelope(t, twoPath, two)
	if _, secret, err := v.Get(b.ID); err == nil || len(secret) != 0 {
		t.Fatal("cross-provider ciphertext transplant was accepted")
	}
}

func TestVaultMissingOrInvalidMasterNeverRegenerates(t *testing.T) {
	for _, pendingOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_only_%t", pendingOnly), func(t *testing.T) {
			v, dir := openFixture(t)
			a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-existing-session"), 0)
			if v.Close() != nil {
				t.Fatal("cannot release synthetic vault ownership")
			}
			accountPath := filepath.Join(dir, a.ID+".account")
			if pendingOnly {
				if err := os.Rename(accountPath, accountPath+".pending"); err != nil {
					t.Fatal("cannot prepare isolated interrupted record")
				}
				accountPath += ".pending"
			}
			before := readFixture(t, accountPath)
			master := filepath.Join(dir, "master.key")
			if err := os.Remove(master); err != nil {
				t.Fatal("cannot remove synthetic master")
			}
			if _, err := accountvault.Open(dir); err == nil {
				t.Fatal("missing master was regenerated over existing encrypted material")
			}
			if _, err := os.Lstat(master); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed Open wrote a replacement master")
			}
			if !bytes.Equal(before, readFixture(t, accountPath)) {
				t.Fatal("failed Open changed existing ciphertext")
			}
		})
	}
	v, dir := openFixture(t)
	putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-existing-session"), 0)
	if v.Close() != nil {
		t.Fatal("cannot release synthetic vault ownership")
	}
	master := filepath.Join(dir, "master.key")
	invalid := []byte("synthetic-invalid-master")
	writeFixture(t, master, invalid)
	if _, err := accountvault.Open(dir); err == nil || !bytes.Equal(invalid, readFixture(t, master)) {
		t.Fatal("invalid master was accepted or replaced")
	}
}

func TestVaultRevisionCASAndProviderOwnership(t *testing.T) {
	v, _ := openFixture(t)
	a := fixtureAccount("fixture-account")
	if _, err := v.Put(a, []byte("synthetic-session"), 3); !errors.Is(err, accountvault.ErrConflict) {
		t.Fatal("nonzero revision created a missing account")
	}
	a.Revision = 999
	first := putFixture(t, v, a, []byte("synthetic-initial-session"), 0)
	if first.Revision != 1 {
		t.Fatal("caller-controlled revision bypassed creation CAS")
	}
	if _, err := v.Put(first, []byte("synthetic-stale-session"), 0); !errors.Is(err, accountvault.ErrConflict) {
		t.Fatal("stale creation revision was accepted")
	}
	other := first
	other.Provider = "other-provider"
	if _, err := v.Put(other, []byte("synthetic-other-provider-session"), 1); !errors.Is(err, accountvault.ErrConflict) {
		t.Fatal("an account identifier was reassigned to another provider")
	}
	got, plain, err := v.Get(first.ID)
	if err != nil || got.Provider != first.Provider || got.Revision != 1 || !bytes.Equal(plain, []byte("synthetic-initial-session")) {
		t.Fatal("rejected CAS changed the account")
	}
	first.Enabled = false
	second := putFixture(t, v, first, []byte("synthetic-new-session"), 1)
	if second.Revision != 2 || second.Enabled {
		t.Fatal("successful CAS did not atomically update metadata")
	}
}

func TestVaultRejectsInvalidInputWithoutWritingRecords(t *testing.T) {
	v, dir := openFixture(t)
	for _, id := range []string{"", "../outside", "x/y", `x\y`, "x:y", strings.Repeat("x", 129)} {
		if _, err := v.Put(fixtureAccount(id), []byte("synthetic-session"), 0); err == nil {
			t.Error("unsafe account identifier was accepted")
		}
	}
	bad := fixtureAccount("fixture-account")
	bad.Provider = "../other"
	if _, err := v.Put(bad, []byte("synthetic-session"), 0); err == nil {
		t.Error("unsafe provider identifier was accepted")
	}
	for _, secret := range [][]byte{nil, make([]byte, (1<<20)+1)} {
		if _, err := v.Put(fixtureAccount("fixture-account"), secret, 0); err == nil {
			t.Error("empty or oversized secret was accepted")
		}
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.account*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("invalid input left record or pending files")
	}
}

func symlinkFixture(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skip("host cannot create symlinks for this test")
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestVaultRejectsLinkedRootAndMaster(t *testing.T) {
	root := fixtureRoot(t)
	out := filepath.Join(root, "outside")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("cannot create isolated outside directory")
	}
	link := filepath.Join(root, "alias")
	symlinkFixture(t, out, link)
	if _, err := accountvault.Open(filepath.Join(link, "vault")); err == nil {
		t.Fatal("Open followed a linked ancestor")
	}
	if _, err := os.Stat(filepath.Join(out, "vault")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected root created data outside its path")
	}
	v, dir := openFixture(t)
	putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-session"), 0)
	if v.Close() != nil {
		t.Fatal("cannot release synthetic vault ownership")
	}
	master := filepath.Join(dir, "master.key")
	outMaster := filepath.Join(root, "outside-master")
	writeFixture(t, outMaster, readFixture(t, master))
	if err := os.Remove(master); err != nil {
		t.Fatal("cannot prepare isolated linked master")
	}
	symlinkFixture(t, outMaster, master)
	if _, err := accountvault.Open(dir); err == nil {
		t.Fatal("Open followed a linked master key")
	}
}

func TestVaultRejectsLinkedAccountAndNeverFollowsPendingLink(t *testing.T) {
	t.Run("account", func(t *testing.T) {
		v, dir := openFixture(t)
		a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-session"), 0)
		path := filepath.Join(dir, a.ID+".account")
		outside := filepath.Join(filepath.Dir(dir), "outside-record")
		before := readFixture(t, path)
		writeFixture(t, outside, before)
		if err := os.Remove(path); err != nil {
			t.Fatal("cannot prepare isolated linked account")
		}
		symlinkFixture(t, outside, path)
		if _, _, err := v.Get(a.ID); err == nil {
			t.Error("Get followed linked account")
		}
		if _, err := v.List(); err == nil {
			t.Error("List followed linked account")
		}
		if _, err := v.Put(a, []byte("synthetic-replacement"), 1); err == nil {
			t.Error("Put accepted linked account")
		}
		if !bytes.Equal(before, readFixture(t, outside)) {
			t.Fatal("linked outside record was changed")
		}
	})
	t.Run("pending", func(t *testing.T) {
		v, dir := openFixture(t)
		a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-initial"), 0)
		outside := filepath.Join(filepath.Dir(dir), "outside-sentinel")
		sentinel := []byte("synthetic-outside-sentinel")
		writeFixture(t, outside, sentinel)
		symlinkFixture(t, outside, filepath.Join(dir, a.ID+".account.pending"))
		_, putErr := v.Put(a, []byte("synthetic-next"), 1)
		if !bytes.Equal(sentinel, readFixture(t, outside)) {
			t.Fatal("Put followed or truncated a pending-file link")
		}
		got, plain, err := v.Get(a.ID)
		if err != nil {
			t.Fatal("pending collision damaged the valid account")
		}
		if putErr != nil && (got.Revision != 1 || !bytes.Equal(plain, []byte("synthetic-initial"))) {
			t.Fatal("failed pending write changed the valid account")
		}
	})
}

func TestVaultCASWithConcurrentCallersAndOwnershipHandover(t *testing.T) {
	v, dir := openFixture(t)
	a := putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-initial"), 0)
	const workers = 12
	for i := 0; i < workers; i++ {
		other, err := accountvault.Open(dir)
		if err == nil {
			_ = other.Close()
			t.Fatal("a second instance acquired active vault ownership")
		}
	}
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := v.Put(a, []byte(fmt.Sprintf("synthetic-worker-%d", i)), 1)
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, accountvault.ErrConflict) {
			t.Error("independent instance failed outside CAS conflict contract")
		}
	}
	if wins != 1 {
		t.Fatalf("independent CAS winners = %d, require exactly one", wins)
	}
	got, _, err := v.Get(a.ID)
	if err != nil || got.Revision != 2 {
		t.Fatal("CAS winner did not persist a readable revision")
	}
	if v.Close() != nil {
		t.Fatal("cannot close first owner")
	}
	second, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal("next owner could not acquire released vault")
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := second.Put(a, []byte("synthetic-stale-owner"), 1); !errors.Is(err, accountvault.ErrConflict) {
		t.Fatal("ownership handover lost persisted CAS revision")
	}
}

func TestVaultClosedHandleCannotReadListOrWrite(t *testing.T) {
	v, _ := openFixture(t)
	if v.Close() != nil || v.Close() != nil {
		t.Fatal("Close must be idempotent")
	}
	if _, err := v.List(); err == nil {
		t.Error("closed empty vault still returned a successful List")
	}
	if _, _, err := v.Get("fixture-account"); err == nil {
		t.Error("closed vault allowed Get")
	}
	if _, err := v.Put(fixtureAccount("fixture-account"), []byte("synthetic-after-close"), 0); err == nil {
		t.Error("closed vault allowed Put")
	}
}

// Child workers print only synchronization/result markers, never records or keys.
func TestMain(m *testing.M) {
	mode := os.Getenv("LOOMGATE_VAULT_TEST_WORKER")
	if mode == "" {
		os.Exit(m.Run())
	}
	dir := os.Getenv("LOOMGATE_VAULT_TEST_DIR")
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(dir), ".synthetic-test-case"))
	if err != nil || !bytes.Equal(marker, []byte("synthetic")) || filepath.Base(dir) != "vault" {
		os.Exit(10)
	}
	v, err := accountvault.Open(dir)
	if err != nil {
		if err.Error() == "vault_busy" {
			fmt.Println("busy")
			os.Exit(0)
		}
		fmt.Println("open-failed")
		os.Exit(11)
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(12)
	}
	id, expected := "fixture-account", uint64(1)
	if mode == "initialize" {
		id, expected = os.Getenv("LOOMGATE_VAULT_TEST_ID"), 0
	}
	_, err = v.Put(fixtureAccount(id), []byte("synthetic-process-"+id), expected)
	switch {
	case err == nil:
		fmt.Println("success")
	case errors.Is(err, accountvault.ErrConflict):
		fmt.Println("conflict")
	default:
		fmt.Println("write-failed")
		os.Exit(13)
	}
	_ = v.Close()
	os.Exit(0)
}

func TestVaultCrossProcessCASAndInitialization(t *testing.T) {
	for _, mode := range []string{"cas", "initialize"} {
		t.Run(mode, func(t *testing.T) {
			root := fixtureRoot(t)
			dir := filepath.Join(root, "vault")
			if mode == "cas" {
				v, err := accountvault.Open(dir)
				if err != nil {
					t.Fatal("cannot prepare cross-process CAS fixture")
				}
				putFixture(t, v, fixtureAccount("fixture-account"), []byte("synthetic-initial"), 0)
				if v.Close() != nil {
					t.Fatal("cannot release seeded vault ownership")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			type worker struct {
				cmd   *exec.Cmd
				input io.WriteCloser
				out   *bufio.Reader
			}
			var workers []worker
			for i := 0; i < 4; i++ {
				cmd := exec.CommandContext(ctx, os.Args[0])
				hideWorker(cmd)
				cmd.Env = append(os.Environ(), "LOOMGATE_VAULT_TEST_WORKER="+mode,
					"LOOMGATE_VAULT_TEST_DIR="+dir, fmt.Sprintf("LOOMGATE_VAULT_TEST_ID=fixture-worker-%d", i))
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal("cannot create worker input")
				}
				stdout, err := cmd.StdoutPipe()
				if err != nil || cmd.Start() != nil {
					t.Fatal("cannot start synthetic vault worker")
				}
				t.Cleanup(func() {
					if cmd.Process != nil && cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				})
				workers = append(workers, worker{cmd: cmd, input: input, out: bufio.NewReader(stdout)})
			}
			var owners []worker
			busy := 0
			for _, w := range workers {
				line, err := w.out.ReadString('\n')
				if err == nil && strings.TrimSpace(line) == "busy" {
					busy++
					_ = w.input.Close()
					if w.cmd.Wait() != nil {
						t.Error("denied worker did not exit cleanly")
					}
					continue
				}
				if err != nil || strings.TrimSpace(line) != "ready" {
					cancel()
					for _, running := range workers {
						_ = running.cmd.Wait()
					}
					t.Fatal("cross-process initialization failed outside ownership contract")
				}
				owners = append(owners, w)
			}
			if len(owners) != 1 || busy != len(workers)-1 {
				t.Error("vault did not maintain one exclusive process owner")
			}
			for _, w := range owners {
				_, _ = io.WriteString(w.input, "go\n")
				_ = w.input.Close()
			}
			wins := 0
			for _, w := range owners {
				line, err := w.out.ReadString('\n')
				waitErr := w.cmd.Wait()
				if err != nil || waitErr != nil {
					t.Error("cross-process vault worker failed")
					continue
				}
				switch strings.TrimSpace(line) {
				case "success":
					wins++
				case "conflict":
				default:
					t.Error("unexpected cross-process result marker")
				}
			}
			want := 1
			if wins != want {
				t.Fatalf("cross-process winners = %d, require %d", wins, want)
			}
			v, err := accountvault.Open(dir)
			if err != nil {
				t.Fatal("cannot reopen cross-process vault")
			}
			t.Cleanup(func() { _ = v.Close() })
			rows, err := v.List()
			if err != nil || len(rows) != want {
				t.Fatal("cross-process writes are missing or unreadable")
			}
			for _, row := range rows {
				_, plain, err := v.Get(row.ID)
				if err != nil || !bytes.Equal(plain, []byte("synthetic-process-"+row.ID)) {
					t.Fatal("concurrent initialization produced incompatible encryption keys")
				}
			}
		})
	}
}
