package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP per RFC 6238: HMAC-SHA1, 6 digits, 30-second steps. SHA1 is what every
// authenticator app supports; HOTP's truncation makes it safe here.
const (
	totpDigits = 6
	totpPeriod = 30
	totpSkew   = 1 // accept one step either side for clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string { return b32.EncodeToString(randomBytes(20)) }

// TOTPURI builds the otpauth:// URI rendered as a QR code during enrollment.
func TOTPURI(secret, issuer, account string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(totpDigits))
	v.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// VerifyTOTP returns the matching time step (for replay protection) and
// whether the code is valid at time t.
func VerifyTOTP(secret, code string, t time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return 0, false
	}
	step := t.Unix() / totpPeriod
	for d := -totpSkew; d <= totpSkew; d++ {
		s := step + int64(d)
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s))), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// NewRecoveryCodes returns n human-friendly single-use codes (xxxxx-xxxxx).
func NewRecoveryCodes(n int) []string {
	const alpha = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]string, n)
	for i := range out {
		b := randomBytes(10)
		var sb strings.Builder
		for j, c := range b {
			if j == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(alpha[int(c)%len(alpha)])
		}
		out[i] = sb.String()
	}
	return out
}

// HashRecoveryCode normalizes and hashes a recovery code. Codes carry ~49
// bits of entropy and are single-use, so a fast hash is adequate.
func HashRecoveryCode(code string) string {
	c := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
	if !strings.Contains(c, "-") && len(c) == 10 {
		c = c[:5] + "-" + c[5:]
	}
	sum := sha256.Sum256([]byte("rowsmith-recovery:" + c))
	return hex.EncodeToString(sum[:])
}
