package auth

import (
	"testing"
	"time"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, rehash := VerifyPassword(h, "correct horse battery staple"); !ok || rehash {
		t.Fatalf("verify ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := VerifyPassword(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestPasswordPolicy(t *testing.T) {
	if CheckPasswordPolicy("short", "a@b.c") == nil {
		t.Fatal("short password accepted")
	}
	if CheckPasswordPolicy("alice-rocks-2026", "alice@example.com") == nil {
		t.Fatal("password containing email local part accepted")
	}
	if CheckPasswordPolicy("aaaaaaaaaaaaaaa", "x@y.z") == nil {
		t.Fatal("repeated char accepted")
	}
	if err := CheckPasswordPolicy("Tidal-Anchor-Orbit-42", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
}

// RFC 6238 Appendix B test vector (SHA1, T=59s -> 94287082, 6 digits = 287082).
func TestTOTPVector(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	step, ok := VerifyTOTP(secret, "287082", time.Unix(59, 0))
	if !ok || step != 1 {
		t.Fatalf("vector failed: step=%d ok=%v", step, ok)
	}
	if _, ok := VerifyTOTP(secret, "287083", time.Unix(59, 0)); ok {
		t.Fatal("bad code accepted")
	}
}

func TestRecoveryCodeNormalization(t *testing.T) {
	codes := NewRecoveryCodes(2)
	c := codes[0]
	if HashRecoveryCode(c) != HashRecoveryCode(" "+c[:5]+c[6:]+" ") {
		t.Fatal("normalization mismatch")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(60, 2)
	a, _ := l.Allow("k")
	b, _ := l.Allow("k")
	c, wait := l.Allow("k")
	if !a || !b || c || wait <= 0 {
		t.Fatalf("limiter sequence %v %v %v %v", a, b, c, wait)
	}
}
