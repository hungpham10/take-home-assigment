package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// Field-level encryption at rest — mirrors opsense-rs opsense-mlib/sops.rs:
// blob = 12-byte random nonce || AES-256-GCM ciphertext(+tag).
// Used for sensitive values that must be recoverable (e.g. abt_ plaintext
// for admin reveal), stored encrypted in the DB with a master key sourced
// from the SOPS-decrypted environment.
var (
	// ErrTooShort mirrors sops.rs "Data too short for decoding".
	ErrTooShort = errors.New("secrets: data too short for decoding")
	// ErrDecrypt mirrors sops.rs "Decode failed".
	ErrDecrypt = errors.New("secrets: decode failed")
)

// MasterKeyEnv is the env var holding the hex-encoded 32-byte master key.
// The value comes from the SOPS-encrypted env file (LTP_MASTER_KEY).
const MasterKeyEnv = "LTP_MASTER_KEY"

// LoadMasterKey reads the master key from the environment. Returns nil when
// unset (caller decides whether that is fatal; features needing it degrade).
func LoadMasterKey() []byte {
	v := os.Getenv(MasterKeyEnv)
	if v == "" {
		return nil
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != 32 {
		return nil
	}
	return key
}

// Encrypt seals plaintext with the master key: nonce(12) || ciphertext.
func Encrypt(masterKey []byte, plaintext string) ([]byte, error) {
	gcm, err := cipherFor(masterKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: rand: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return append(nonce, sealed...), nil
}

// Decrypt opens a nonce(12)||ciphertext blob produced by Encrypt.
func Decrypt(masterKey, encrypted []byte) (string, error) {
	if len(encrypted) < 12 {
		return "", ErrTooShort
	}
	gcm, err := cipherFor(masterKey)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, encrypted[:12], encrypted[12:], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plaintext), nil
}

func cipherFor(masterKey []byte) (cipher.AEAD, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("secrets: master key must be 32 bytes")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes: %w", err)
	}
	return cipher.NewGCM(block)
}

// GenerateMasterKey returns a fresh 32-byte key hex-encoded (ops bootstrap).
func GenerateMasterKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
