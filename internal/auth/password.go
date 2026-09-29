// Package auth holds the credential primitives: password hashing, TOTP,
// recovery codes, session tokens and login rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. 64 MiB / 3 passes / 2 lanes exceeds the OWASP
// minimum (19 MiB, t=2, p=1) while keeping a login under ~100 ms.
const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16

	MinPasswordLen = 12
	MaxPasswordLen = 256
)

var ErrWeakPassword = errors.New("password must be 12 to 256 characters and must not contain your email")

func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a PHC-formatted argon2id hash. needsRehash
// is true when the hash uses weaker parameters than the current defaults.
func VerifyPassword(hash, pw string) (ok, needsRehash bool) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, false
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false
	}
	return true, m < argonMemory || t < argonTime || p < argonThreads
}

// dummyHash is verified against when the account does not exist, so that
// response timing does not reveal which emails are registered.
var dummyHash, _ = HashPassword("rowsmith-timing-equalizer-" + base64.RawStdEncoding.EncodeToString(randomBytes(12)))

func BurnPasswordCheck(pw string) { VerifyPassword(dummyHash, pw) }

func CheckPasswordPolicy(pw, email string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen || n > MaxPasswordLen {
		return ErrWeakPassword
	}
	if local, _, ok := strings.Cut(strings.ToLower(email), "@"); ok && len(local) >= 4 && strings.Contains(strings.ToLower(pw), local) {
		return ErrWeakPassword
	}
	if isCommonPassword(pw) {
		return errors.New("this password appears in lists of commonly used passwords")
	}
	return nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

var common = map[string]bool{}

func init() {
	for _, p := range strings.Fields(`password123 password1234 123456789012 qwertyuiop12 administrator
		iloveyou1234 letmein12345 welcome12345 passw0rd1234 changeme1234 qwerty123456 1q2w3e4r5t6y
		adminadmin12 password!123 p@ssw0rd1234 superman1234 trustno11234 football1234 monkey123456
		abc123456789 aaaaaaaaaaaa 111111111111 000000000000 123123123123 qazwsxedcrfv zaq12wsxcde3`) {
		common[p] = true
	}
}

func isCommonPassword(pw string) bool {
	lp := strings.ToLower(pw)
	if common[lp] {
		return true
	}
	// Reject a single repeated character.
	first, _ := utf8.DecodeRuneInString(lp)
	return strings.Trim(lp, string(first)) == ""
}
