package secrets

import (
	"strings"
	"testing"
)

const testMasterKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSopsEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv(MasterKeyEnv, testMasterKey)

	plaintext := "client-secret-abc-123!!"
	blob, err := EncryptHex(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Postgres bytea literal: prefix \x + lowercase hex.
	if !strings.HasPrefix(blob, "\\x") {
		t.Fatalf("blob = %q, muốn prefix \\x", blob)
	}
	if len(blob) != 2+12*2+len(plaintext)*2+32 { // \x + nonce(24hex) + ciphertext incl tag
		t.Fatalf("blob len = %d, bất thường", len(blob))
	}

	got, err := DecryptHex(blob)
	if err != nil {
		t.Fatalf("decrypt (\\): %v", err)
	}
	if got != plaintext {
		t.Fatalf("decrypt (\\) = %q, muốn %q", got, plaintext)
	}

	// Nhận cả 0xHEX và hex trần.
	hexOnly := strings.TrimPrefix(blob, "\\x")
	got0x, err := DecryptHex("0x" + hexOnly)
	if err != nil || got0x != plaintext {
		t.Fatalf("decrypt (0x) = (%q, %v)", got0x, err)
	}
	gotBare, err := DecryptHex(hexOnly)
	if err != nil || gotBare != plaintext {
		t.Fatalf("decrypt (bare) = (%q, %v)", gotBare, err)
	}

	// Hai lần encrypt khác nhau (nonce ngẫu nhiên) nhưng decrypt cùng plaintext.
	blob2, err := EncryptHex(plaintext)
	if err != nil {
		t.Fatalf("encrypt2: %v", err)
	}
	if blob == blob2 {
		t.Error("nonce phải ngẫu nhiên — hai blob phải khác nhau")
	}
	if got2, _ := DecryptHex(blob2); got2 != plaintext {
		t.Fatalf("decrypt blob2 = %q, muốn %q", got2, plaintext)
	}
}

func TestSopsEncryptErrors(t *testing.T) {
	t.Setenv(MasterKeyEnv, testMasterKey)

	// Plaintext rỗng vẫn mã hoá được (AES-GCM của chuỗi rỗng).
	if blob, err := EncryptHex(""); err != nil || !strings.HasPrefix(blob, "\\x") {
		t.Fatalf("encrypt empty = (%q, %v)", blob, err)
	}
}

func TestSopsDecodeErrors(t *testing.T) {
	t.Setenv(MasterKeyEnv, testMasterKey)

	cases := []struct {
		name    string
		payload string
		wantSub string
	}{
		{"hex hỏng", "not-hex-at-all!!", "decode hex failed"},
		{"blob quá ngắn (<12 byte)", "\\xabcd", "too short for decoding"},
		{"ciphertext sai (≥12 byte, tag không khớp)", "\\x" + strings.Repeat("aa", 24), "decode failed"},
		{"không có master key (unset)", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "không có master key (unset)" {
				t.Setenv(MasterKeyEnv, "")
			}
			_, err := DecryptHex(c.payload)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("err = %v, muốn chứa %q", err, c.wantSub)
			}
		})
	}
}

func TestLoadMasterKeyErrors(t *testing.T) {
	cases := []struct {
		name    string
		envVal  string
		wantSub string
	}{
		{"unset", "", "chưa set"},
		{"không phải hex", "xyz", "không phải hex hợp lệ"},
		{"sai độ dài (16 byte)", strings.Repeat("a", 32), "phải 32 byte"},
		{"đúng (32 byte hex)", testMasterKey, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.envVal == "" {
				t.Setenv(MasterKeyEnv, "")
			} else {
				t.Setenv(MasterKeyEnv, c.envVal)
			}
			_, err := loadMasterKeyHex()
			if c.wantSub == "" {
				if err != nil {
					t.Fatalf("không muốn lỗi, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("err = %v, muốn chứa %q", err, c.wantSub)
			}
		})
	}
}
