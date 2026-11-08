package script

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Validator checks a staged k6 script. LocalK6 runs `k6 inspect`; the
// structural fallback keeps the lifecycle testable without the k6 binary.
type Validator interface {
	Validate(ctx context.Context, content []byte) (errorsJSON []string, err error)
	Name() string
}

// LocalK6 validates via `k6 inspect` (parses the script without executing).
type LocalK6 struct{ Bin string }

// Name identifies the validator in validation metadata.
func (LocalK6) Name() string { return "k6-inspect" }

// Validate runs k6 inspect on the staged content.
func (v LocalK6) Validate(ctx context.Context, content []byte) ([]string, error) {
	bin := v.Bin
	if bin == "" {
		bin = "k6"
	}
	tmp, err := writeFileTemp(content)
	if err != nil {
		return nil, err
	}
	defer func() { _ = removeFile(tmp) }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "inspect", tmp).CombinedOutput()
	if err != nil {
		return []string{fmt.Sprintf("k6 inspect: %s", truncate(out, 4000))}, nil
	}
	return nil, nil
}

// Structural is the no-k6 fallback: catches obviously broken uploads so the
// lifecycle still produces a deterministic terminal status.
type Structural struct{}

// Name identifies the validator in validation metadata.
func (Structural) Name() string { return "structural" }

// Validate applies structural checks without external tooling.
func (Structural) Validate(_ context.Context, content []byte) ([]string, error) {
	var errs []string
	if len(content) == 0 {
		errs = append(errs, "script is empty")
	}
	if !json.Valid(nil) && len(content) > 0 && content[0] == 0 {
		errs = append(errs, "binary content is not a k6 script")
	}
	return errs, nil
}

// CurrentValidator picks k6 when available, else structural fallback.
func CurrentValidator() Validator {
	if _, err := exec.LookPath("k6"); err == nil {
		return LocalK6{}
	}
	return Structural{}
}

// ValidateNow runs validation for one script: VALIDATING → VALIDATED|INVALID|
// VALIDATION_FAILED, recording the attempt in tbl_script_validations. The
// caller (HTTP handler on enqueue-accept, or the nil-queue sync path) owns the
// →VALIDATING transition; ValidateWith only records + transitions to terminal.
func ValidateNow(ctx context.Context, db *gorm.DB, scriptsDir, scriptID string) error {
	return ValidateWith(ctx, db, scriptsDir, scriptID, CurrentValidator())
}

// ValidateWith runs a specific validator and records the attempt + the
// terminal transition. The script must already be in VALIDATING.
func ValidateWith(ctx context.Context, db *gorm.DB, scriptsDir, scriptID string, v Validator) error {
	var sc store.Script
	if err := db.First(&sc, "id = ?", scriptID).Error; err != nil {
		return err
	}
	content, readErr := os.ReadFile(scriptsJoin(scriptsDir, sc.ContentRef))
	if readErr != nil {
		// Staging file lost — never leave the script stuck: record the attempt
		// and transition VALIDATING → VALIDATION_FAILED.
		return recordTerminal(ctx, db, sc.ID, v.Name(), "VALIDATION_FAILED",
			store.JSONBytes([]string{"staged script unreadable: " + readErr.Error()}))
	}
	errs, verr := v.Validate(ctx, content)

	var to string
	errsJSON := store.JSONBytes([]string{})
	if verr != nil {
		// Infrastructure error (k6 missing/crash) — distinct terminal status.
		to = "VALIDATION_FAILED"
		errsJSON = store.JSONBytes([]string{verr.Error()})
	} else if len(errs) > 0 {
		to = "INVALID"
		errsJSON = store.JSONBytes(errs)
	} else {
		to = "VALIDATED"
	}
	return recordTerminal(ctx, db, sc.ID, v.Name(), to, errsJSON)
}

// recordTerminal inserts the tbl_script_validations attempt and performs the
// VALIDATING → to transition in one transaction.
func recordTerminal(ctx context.Context, db *gorm.DB, scriptID, validator, to string, errsJSON []byte) error {
	attempt := 1
	var last store.ScriptValidation
	if err := db.Where("script_id = ?", scriptID).Order("attempt DESC").First(&last).Error; err == nil {
		attempt = last.Attempt + 1
	}
	rec := store.ScriptValidation{ScriptID: scriptID, Attempt: attempt, Status: to, Errors: errsJSON}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
		// Wrapper central (pkgs/store/script_lifecycle.go) — không raw Transition.
		var terr error
		switch to {
		case store.ScriptStatusValidated:
			terr = store.SCRToValidated(ctx, tx, scriptID, store.ActorJSON("", "system", ""), validator)
		case store.ScriptStatusInvalid:
			terr = store.SCRToInvalid(ctx, tx, scriptID, store.ActorJSON("", "system", ""), validator)
		default:
			terr = store.SCRToValidationFailed(ctx, tx, scriptID, store.ActorJSON("", "system", ""), validator)
		}
		return terr
	})
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
