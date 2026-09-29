package vault

import (
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	v, err := New(GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	env, err := v.Seal([]byte("hunter2"), "connection:abc:secrets")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env, "hunter2") {
		t.Fatal("plaintext leaked into envelope")
	}
	got, err := v.Open(env, "connection:abc:secrets")
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("open = %q, %v", got, err)
	}
}

func TestAADBinding(t *testing.T) {
	v, _ := New(GenerateKey())
	env, _ := v.Seal([]byte("secret"), "connection:a:secrets")
	if _, err := v.Open(env, "connection:b:secrets"); err != ErrDecrypt {
		t.Fatalf("expected ErrDecrypt when AAD differs, got %v", err)
	}
}

func TestTamperDetected(t *testing.T) {
	v, _ := New(GenerateKey())
	env, _ := v.Seal([]byte("secret"), "x")
	b := []byte(env)
	b[len(b)-3] ^= 1
	if _, err := v.Open(string(b), "x"); err == nil {
		t.Fatal("tampered envelope decrypted")
	}
}

func TestRotationAndRewrap(t *testing.T) {
	oldKey, newKey := GenerateKey(), GenerateKey()
	vOld, _ := New(oldKey)
	env, _ := vOld.Seal([]byte("payload"), "aad")

	vNew, _ := New(newKey, oldKey)
	if !vNew.NeedsRewrap(env) {
		t.Fatal("expected envelope to need rewrap")
	}
	if got, err := vNew.Open(env, "aad"); err != nil || string(got) != "payload" {
		t.Fatalf("old envelope should still open: %q %v", got, err)
	}
	re, err := vNew.Rewrap(env, "aad")
	if err != nil {
		t.Fatal(err)
	}
	if vNew.NeedsRewrap(re) {
		t.Fatal("rewrapped envelope still points at old key")
	}
	vOnlyNew, _ := New(newKey)
	if got, err := vOnlyNew.Open(re, "aad"); err != nil || string(got) != "payload" {
		t.Fatalf("rewrapped envelope should open with new key alone: %q %v", got, err)
	}
	if _, err := vOnlyNew.Open(env, "aad"); err != ErrUnknownKey {
		t.Fatalf("expected ErrUnknownKey, got %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	k := GenerateKey()
	keys, err := ParseKeys("# comment\n\n" + EncodeKey(k) + "\n")
	if err != nil || len(keys) != 1 || string(keys[0]) != string(k) {
		t.Fatalf("ParseKeys: %v %v", keys, err)
	}
}
