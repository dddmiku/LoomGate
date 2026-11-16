package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
)

// CredentialRevision is an opaque comparison key; never includes raw tokens
// in task state or logs. It also binds the original file and realm identity.
func (a *Auth) CredentialRevision() string {
	s := a.Snapshot()
	h := sha256.New()
	for _, v := range []string{s.UID, s.AccessToken, s.RefreshToken, s.Domain, s.realm, s.FilePath, s.DeviceToken, strconv.FormatInt(s.ExpiresAt, 10)} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CommitTokenUpdate persists the new credentials before publishing them to
// readers. CAS prevents a slow refresh from overwriting concurrent re-login.
// The short filesystem commit holds the credential lock; network I/O never does.
func (a *Auth) CommitTokenUpdate(expected string, update TokenUpdate) error {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	next := &Auth{UID: a.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt, Domain: a.Domain, realm: a.realm, FilePath: a.FilePath, DeviceToken: a.DeviceToken, EnterpriseID: a.EnterpriseID, Nickname: a.Nickname}
	if next.CredentialRevision() != expected {
		return errors.New("credential_revision_conflict")
	}
	stored, err := next.PersistedRevision()
	if err != nil || stored != expected {
		return errors.New("credential_file_changed")
	}
	if strings.TrimSpace(update.AccessToken) == "" || update.ExpiresAt <= 0 {
		return errors.New("credential_update_invalid")
	}
	next.AccessToken = update.AccessToken
	if update.RefreshToken != "" {
		next.RefreshToken = update.RefreshToken
	}
	if update.Domain != "" {
		next.Domain = update.Domain
	}
	next.ExpiresAt = update.ExpiresAt
	if next.realm == "" {
		next.realm = ResolveRealm("", next.Domain)
	}
	if err := saveSnapshot(next); err != nil {
		return errors.New("credential_save_failed")
	}
	a.AccessToken = next.AccessToken
	a.RefreshToken = next.RefreshToken
	a.Domain = next.Domain
	a.ExpiresAt = next.ExpiresAt
	a.realm = next.realm
	return nil
}

// PersistedRevision checks the actual safe account file, not the in-memory
// expiry alone. Raw bytes are cleared and parse/file errors never escape.
func (a *Auth) PersistedRevision() (string, error) {
	s := a.Snapshot()
	info, err := os.Lstat(s.FilePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", errors.New("credential_file_unavailable")
	}
	raw, err := os.ReadFile(s.FilePath)
	if err != nil {
		return "", errors.New("credential_file_unavailable")
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	parsed, err := Parse(raw)
	if err != nil {
		return "", errors.New("credential_file_invalid")
	}
	parsed.FilePath = s.FilePath
	return parsed.CredentialRevision(), nil
}
