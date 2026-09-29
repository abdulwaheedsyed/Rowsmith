// Package vault implements envelope encryption for secrets at rest.
//
// Every sealed value gets its own random 256-bit data key (DEK). The payload is
// encrypted with XChaCha20-Poly1305 under the DEK, and the DEK is wrapped with a
// key-encryption key (KEK) derived from the master key via HKDF-SHA256. Both
// layers bind "additional authenticated data" (AAD) that names the record and
// field the secret belongs to, so a ciphertext copied into another row fails to
// decrypt instead of silently leaking into the wrong context.
//
// Envelope format (ASCII, safe for TEXT columns):
//
//	rsv1$<key id>$<b64 nonce||wrapped DEK>$<b64 nonce||ciphertext>
//
// Key rotation keeps older master keys available for decryption; Rewrap moves
// an envelope to the current key without touching the payload.
package vault

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	prefix  = "rsv1"
	keySize = 32
)

var (
	ErrMalformed = errors.New("vault: malformed envelope")
	ErrUnknownKey = errors.New("vault: envelope sealed with an unknown master key")
	ErrDecrypt   = errors.New("vault: decryption failed (wrong key or tampered data)")
)

type kek struct {
	id  string
	key []byte
}

type Vault struct {
	current kek
	byID    map[string]kek
	master  []byte // current master key, for Derive
}

// New builds a vault from one or more raw 32-byte master keys. The first key
// seals new data; all keys can open existing data.
func New(masterKeys ...[]byte) (*Vault, error) {
	if len(masterKeys) == 0 {
		return nil, errors.New("vault: no master key")
	}
	v := &Vault{byID: map[string]kek{}}
	for i, mk := range masterKeys {
		if len(mk) != keySize {
			return nil, fmt.Errorf("vault: master key %d must be %d bytes, got %d", i, keySize, len(mk))
		}
		k, err := hkdf.Key(sha256.New, mk, nil, "rowsmith/kek/v1", keySize)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(k)
		entry := kek{id: hex.EncodeToString(sum[:4]), key: k}
		if i == 0 {
			v.current = entry
			v.master = mk
		}
		v.byID[entry.id] = entry
	}
	return v, nil
}

// KeyID identifies the current master key (a short fingerprint, not secret).
func (v *Vault) KeyID() string { return v.current.id }

// Derive returns a purpose-specific subkey of the current master key.
func (v *Vault) Derive(purpose string) []byte {
	k, err := hkdf.Key(sha256.New, v.master, nil, "rowsmith/"+purpose, keySize)
	if err != nil {
		panic(err) // only fails for absurd lengths
	}
	return k
}

func (v *Vault) Seal(plaintext []byte, aad string) (string, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return "", err
	}
	defer clear(dek)

	body, err := seal(dek, plaintext, []byte(aad))
	if err != nil {
		return "", err
	}
	wrapped, err := seal(v.current.key, dek, []byte("dek|"+aad))
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return strings.Join([]string{prefix, v.current.id, enc.EncodeToString(wrapped), enc.EncodeToString(body)}, "$"), nil
}

func (v *Vault) Open(envelope, aad string) ([]byte, error) {
	k, wrapped, body, err := v.parse(envelope)
	if err != nil {
		return nil, err
	}
	dek, err := open(k.key, wrapped, []byte("dek|"+aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	defer clear(dek)
	pt, err := open(dek, body, []byte(aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// Rewrap re-encrypts only the data key under the current master key.
// It returns the input unchanged when it is already current.
func (v *Vault) Rewrap(envelope, aad string) (string, error) {
	k, wrapped, body, err := v.parse(envelope)
	if err != nil {
		return "", err
	}
	if k.id == v.current.id {
		return envelope, nil
	}
	dek, err := open(k.key, wrapped, []byte("dek|"+aad))
	if err != nil {
		return "", ErrDecrypt
	}
	defer clear(dek)
	// Verify the payload still opens before committing to the new wrapping.
	if _, err := open(dek, body, []byte(aad)); err != nil {
		return "", ErrDecrypt
	}
	nw, err := seal(v.current.key, dek, []byte("dek|"+aad))
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return strings.Join([]string{prefix, v.current.id, enc.EncodeToString(nw), enc.EncodeToString(body)}, "$"), nil
}

// NeedsRewrap reports whether the envelope was sealed with an older key.
func (v *Vault) NeedsRewrap(envelope string) bool {
	parts := strings.Split(envelope, "$")
	return len(parts) == 4 && parts[0] == prefix && parts[1] != v.current.id
}

func (v *Vault) SealJSON(x any, aad string) (string, error) {
	b, err := json.Marshal(x)
	if err != nil {
		return "", err
	}
	defer clear(b)
	return v.Seal(b, aad)
}

func (v *Vault) OpenJSON(envelope, aad string, x any) error {
	b, err := v.Open(envelope, aad)
	if err != nil {
		return err
	}
	defer clear(b)
	return json.Unmarshal(b, x)
}

func (v *Vault) parse(envelope string) (kek, []byte, []byte, error) {
	parts := strings.Split(envelope, "$")
	if len(parts) != 4 || parts[0] != prefix {
		return kek{}, nil, nil, ErrMalformed
	}
	k, ok := v.byID[parts[1]]
	if !ok {
		return kek{}, nil, nil, ErrUnknownKey
	}
	enc := base64.RawURLEncoding
	wrapped, err1 := enc.DecodeString(parts[2])
	body, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return kek{}, nil, nil, ErrMalformed
	}
	return k, wrapped, body, nil
}

func seal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrMalformed
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, ct, aad)
}

// GenerateKey returns a fresh random master key.
func GenerateKey() []byte {
	k := make([]byte, keySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

// EncodeKey renders a master key for storage in a key file or env variable.
func EncodeKey(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// ParseKeys decodes a key file body: one base64 key per line, '#' comments allowed.
func ParseKeys(data string) ([][]byte, error) {
	var keys [][]byte
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("vault: key file line is not base64: %w", err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("vault: key file contains no keys")
	}
	return keys, nil
}

// LoadOrCreate resolves the master keys. Order: explicit env value, explicit
// file, then <dataDir>/keys/master.key — generated with 0600 permissions on
// first start. The bool result reports whether a key was generated.
func LoadOrCreate(envKey, file, dataDir string) (*Vault, bool, error) {
	if envKey != "" {
		keys, err := ParseKeys(envKey)
		if err != nil {
			return nil, false, err
		}
		v, err := New(keys...)
		return v, false, err
	}
	generated := false
	if file == "" {
		file = filepath.Join(dataDir, "keys", "master.key")
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				return nil, false, err
			}
			body := "# Rowsmith master key. Back this file up separately from the database;\n" +
				"# without it, saved connection secrets cannot be decrypted.\n" +
				EncodeKey(GenerateKey()) + "\n"
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				return nil, false, err
			}
			generated = true
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, false, fmt.Errorf("vault: reading master key: %w", err)
	}
	keys, err := ParseKeys(string(data))
	if err != nil {
		return nil, false, err
	}
	v, err := New(keys...)
	return v, generated, err
}
