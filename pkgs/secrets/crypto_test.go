package secrets

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := randRead(key); err != nil {
		t.Fatal(err)
	}
	const secret = "abt_0123456789abcdef"
	blob, err := Encrypt(key, secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Format mirrors sops.rs: 12-byte nonce prefix + ciphertext(+tag).
	if len(blob) < 12+len(secret)+16 {
		t.Fatalf("blob too short: %d", len(blob))
	}
	got, err := Decrypt(key, blob)
	if err != nil || got != secret {
		t.Fatalf("decrypt = (%q, %v)", got, err)
	}
	// Two encryptions differ (random nonce).
	blob2, _ := Encrypt(key, secret)
	if bytes.Equal(blob, blob2) {
		t.Error("nonces must be random — blobs must differ")
	}
}

func TestDecryptErrors(t *testing.T) {
	key := make([]byte, 32)
	wrongKey := make([]byte, 32)
	if _, err := randRead(wrongKey); err != nil {
		t.Fatal(err)
	}
	blob, _ := Encrypt(key, "x")
	if _, err := Decrypt(wrongKey, blob); err != ErrDecrypt {
		t.Errorf("wrong key: err = %v, want ErrDecrypt", err)
	}
	if _, err := Decrypt(key, []byte("short")); err != ErrTooShort {
		t.Errorf("short blob: err = %v, want ErrTooShort", err)
	}
}

func TestLoadMasterKey(t *testing.T) {
	t.Setenv(MasterKeyEnv, "")
	if LoadMasterKey() != nil {
		t.Error("unset env must return nil")
	}
	hexKey, _ := GenerateMasterKey()
	t.Setenv(MasterKeyEnv, hexKey)
	if got := LoadMasterKey(); len(got) != 32 {
		t.Errorf("LoadMasterKey len = %d, want 32", len(got))
	}
	t.Setenv(MasterKeyEnv, "nothex")
	if LoadMasterKey() != nil {
		t.Error("invalid hex must return nil")
	}
}
