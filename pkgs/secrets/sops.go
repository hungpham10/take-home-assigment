package secrets

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EncryptHex seals plaintext với master key từ env LTP_MASTER_KEY (64 ký tự
// hex = 32 byte) và in ra hex blob dạng Postgres bytea literal (`\xHEX`).
// Mirror opsense-rs crates/opsense-mlib/src/sops.rs (AES-256-GCM, blob =
// nonce(12) ‖ ciphertext) + crates/opsense/src/token.rs (CLI encrypt/decrypt).
//
// Master key phải đúng 32 byte sau hex-decode. Lỗi trả về rõ ràng, KHÔNG panic.
func EncryptHex(plaintext string) (string, error) {
	masterKey, err := loadMasterKeyHex()
	if err != nil {
		return "", err
	}
	blob, err := Encrypt(masterKey, plaintext)
	if err != nil {
		return "", err
	}
	return "\\x" + hex.EncodeToString(blob), nil
}

// DecryptHex giải mã blob (nhận `\xHEX`, `0xHEX`, hoặc hex trần) thành
// plaintext. Dùng để verify seed đã paste đúng.
func DecryptHex(payload string) (string, error) {
	masterKey, err := loadMasterKeyHex()
	if err != nil {
		return "", err
	}
	hexPart := strings.TrimPrefix(payload, "\\x")
	hexPart = strings.TrimPrefix(hexPart, "0x")
	encrypted, err := hex.DecodeString(hexPart)
	if err != nil {
		return "", fmt.Errorf("secrets: decode hex failed: %w", err)
	}
	return Decrypt(masterKey, encrypted)
}

// loadMasterKeyHex đọc LTP_MASTER_KEY (hex 64 ký tự) và decode thành 32 byte.
// Trả lỗi nếu env unset, không phải hex, hoặc sai độ dài.
func loadMasterKeyHex() ([]byte, error) {
	v := os.Getenv(MasterKeyEnv)
	if v == "" {
		return nil, errors.New("secrets: LTP_MASTER_KEY chưa set (SOPS env chưa giải mã hoặc thiếu env)")
	}
	key, err := hex.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("secrets: LTP_MASTER_KEY không phải hex hợp lệ: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: LTP_MASTER_KEY phải 32 byte (64 ký tự hex), hiện %d byte", len(key))
	}
	return key, nil
}

// SopsSubcommand hiện `encrypt` | `decrypt` cho CLI loadtest-controller.
// Thực thi trước config.FromEnv() nên không cần DATABASE_DSN/DB.
type SopsSubcommand struct{}

// Run thực thi `sops <encrypt|decrypt> <payload>` cho CLI loadtest-controller.
func (s SopsSubcommand) Run(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: loadtest-controller sops <encrypt|decrypt> <payload>")
	}
	switch args[0] {
	case "encrypt":
		if len(args) < 2 {
			return errors.New("usage: loadtest-controller sops encrypt <plaintext>")
		}
		out, err := EncryptHex(args[1])
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	case "decrypt":
		if len(args) < 2 {
			return errors.New("usage: loadtest-controller sops decrypt <blob\\xHEX|0xHEX|hex>")
		}
		out, err := DecryptHex(args[1])
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	default:
		return fmt.Errorf("secrets: unknown sops action %q: chỉ hỗ trợ encrypt | decrypt", args[0])
	}
}
