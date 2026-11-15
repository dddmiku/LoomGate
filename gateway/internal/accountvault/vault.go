// Package accountvault stores new provider sessions encrypted at rest. The
// portable master key must be migrated with the vault; it is never regenerated
// when an existing encrypted account cannot be opened.
package accountvault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var ErrConflict = errors.New("account_revision_conflict")

type Account struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	Enabled   bool      `json:"enabled"`
}
type sealed struct {
	Account    Account `json:"account"`
	Nonce      []byte  `json:"nonce"`
	Ciphertext []byte  `json:"ciphertext"`
}
type Vault struct {
	mu     sync.Mutex
	dir    string
	aead   cipher.AEAD
	owner  *os.File
	closed bool
}

func Open(dir string) (*Vault, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("vault_path_invalid")
	}
	if abs == filepath.Dir(abs) {
		return nil, errors.New("vault_path_invalid")
	}
	for path := abs; ; path = filepath.Dir(path) {
		if isLink(path) {
			return nil, errors.New("vault_link_rejected")
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	if err = secureDirectory(abs); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(abs, "master.key")
	if isLink(keyPath) || isLink(filepath.Join(abs, "owner.lock")) {
		return nil, errors.New("vault_link_rejected")
	}
	owner, err := os.OpenFile(filepath.Join(abs, "owner.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("vault_lock_failed")
	}
	if lockOwner(owner) != nil {
		owner.Close()
		return nil, errors.New("vault_busy")
	}
	success := false
	defer func() {
		if !success {
			owner.Close()
		}
	}()
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		rows, _ := filepath.Glob(filepath.Join(abs, "*.account"))
		pending, _ := filepath.Glob(filepath.Join(abs, "*.account.pending"))
		rows = append(rows, pending...)
		if len(rows) > 0 {
			return nil, errors.New("vault_master_key_missing")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, errors.New("vault_entropy_failed")
		}
		f, err := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, errors.New("vault_master_key_create_failed")
		}
		_, werr := f.Write(key)
		serr := f.Sync()
		cerr := f.Close()
		if werr != nil || serr != nil || cerr != nil {
			return nil, errors.New("vault_master_key_write_failed")
		}
	} else if err != nil {
		return nil, errors.New("vault_master_key_unreadable")
	}
	if len(key) != 32 {
		return nil, errors.New("vault_master_key_invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("vault_cipher_failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("vault_cipher_failed")
	}
	success = true
	return &Vault{dir: abs, aead: aead, owner: owner}, nil
}
func (v *Vault) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	return v.owner.Close()
}
func (v *Vault) load(id string) (sealed, error) {
	var s sealed
	if v.closed {
		return s, errors.New("vault_closed")
	}
	if !validID.MatchString(id) {
		return s, errors.New("account_id_invalid")
	}
	path := filepath.Join(v.dir, id+".account")
	if isLink(path) {
		return s, errors.New("vault_link_rejected")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, errors.New("account_unavailable")
	}
	if len(b) > 2<<20 || json.Unmarshal(b, &s) != nil || s.Account.ID != id {
		return s, errors.New("account_record_invalid")
	}
	return s, nil
}
func (v *Vault) List() ([]Account, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, errors.New("vault_closed")
	}
	paths, err := filepath.Glob(filepath.Join(v.dir, "*.account"))
	if err != nil {
		return nil, errors.New("vault_list_failed")
	}
	out := []Account{}
	for _, p := range paths {
		base := filepath.Base(p)
		s, err := v.load(base[:len(base)-len(".account")])
		if err != nil {
			return nil, err
		}
		plain, err := v.decrypt(s)
		if err != nil {
			return nil, err
		}
		for i := range plain {
			plain[i] = 0
		}
		out = append(out, s.Account)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func aad(a Account) []byte { b, _ := json.Marshal(a); return b }
func (v *Vault) Get(id string) (Account, []byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, err := v.load(id)
	if err != nil {
		return Account{}, nil, err
	}
	plain, err := v.decrypt(s)
	return s.Account, plain, err
}

// SetEnabled preserves the encrypted session and requires the observed revision.
func (v *Vault) SetEnabled(id string, enabled bool, expected uint64) (Account, error) {
	a, secret, err := v.Get(id)
	if err != nil {
		return Account{}, err
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	if a.Provider != "doubao" || a.Revision != expected {
		return Account{}, ErrConflict
	}
	a.Enabled = enabled
	return v.Put(a, secret, expected)
}
func (v *Vault) decrypt(s sealed) ([]byte, error) {
	if len(s.Nonce) != v.aead.NonceSize() {
		return nil, errors.New("account_ciphertext_invalid")
	}
	plain, err := v.aead.Open(nil, s.Nonce, s.Ciphertext, aad(s.Account))
	if err != nil {
		return nil, errors.New("account_decryption_failed")
	}
	return plain, nil
}
func (v *Vault) Put(a Account, secret []byte, expected uint64) (Account, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return Account{}, errors.New("vault_closed")
	}
	if !validID.MatchString(a.ID) || !validID.MatchString(a.Provider) || len(secret) == 0 || len(secret) > 1<<20 {
		return Account{}, errors.New("account_input_invalid")
	}
	path := filepath.Join(v.dir, a.ID+".account")
	if _, err := os.Stat(path); err == nil {
		old, err := v.load(a.ID)
		if err != nil {
			return Account{}, err
		}
		plain, err := v.decrypt(old)
		if err != nil {
			return Account{}, err
		}
		for i := range plain {
			plain[i] = 0
		}
		if old.Account.Revision != expected || old.Account.Provider != a.Provider {
			return Account{}, ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) || expected != 0 {
		return Account{}, ErrConflict
	}
	a.Revision = expected + 1
	a.UpdatedAt = time.Now().UTC()
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Account{}, errors.New("vault_entropy_failed")
	}
	s := sealed{Account: a, Nonce: nonce, Ciphertext: v.aead.Seal(nil, nonce, secret, aad(a))}
	b, err := json.Marshal(s)
	if err != nil {
		return Account{}, errors.New("account_encode_failed")
	}
	f, err := os.OpenFile(path+".pending", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Account{}, errors.New("account_write_failed")
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(path+".pending", path)
	}
	if err != nil {
		return Account{}, errors.New("account_write_failed")
	}
	return a, nil
}
