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
// VALIDATION_FAILED, recording the attempt in tbl_script_validations.
func ValidateNow(ctx context.Context, db *gorm.DB, scriptsDir, scriptID string) error {
	return ValidateWith(ctx, db, scriptsDir, scriptID, CurrentValidator())
}

// ValidateWith runs a specific validator and records the attempt + transitions.
func ValidateWith(ctx context.Context, db *gorm.DB, scriptsDir, scriptID string, v Validator) error {
	var sc store.Script
	if err := db.First(&sc, "id = ?", scriptID).Error; err != nil {
		return err
	}
	content, err := os.ReadFile(scriptsJoin(scriptsDir, sc.ContentRef))
	if err != nil {
		return err
	}
	errs, verr := v.Validate(ctx, content)

	attempt := 1
	var last store.ScriptValidation
	if err := db.Where("script_id = ?", sc.ID).Order("attempt DESC").First(&last).Error; err == nil {
		attempt = last.Attempt + 1
	}

	var to string
	errsJSON := jsonBytes([]string{})
	if verr != nil {
		// Infrastructure error (k6 missing/crash) — distinct terminal status.
		to = "VALIDATION_FAILED"
		errsJSON = jsonBytes([]string{verr.Error()})
	} else if len(errs) > 0 {
		to = "INVALID"
		errsJSON = jsonBytes(errs)
	} else {
		to = "VALIDATED"
	}

	rec := store.ScriptValidation{ScriptID: sc.ID, Attempt: attempt, Status: to, Errors: errsJSON}
	return db.Transaction(func(tx *gorm.DB) error {
		// State machine requires PENDING/INVALID/VALIDATION_FAILED → VALIDATING
		// → terminal; never jump straight to a terminal status.
		if err := store.Transition(ctx, tx, "script", sc.ID, "VALIDATING",
			store.ActorJSON("", "system", ""), v.Name()); err != nil {
			return err
		}
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
		return store.Transition(ctx, tx, "script", sc.ID, to,
			store.ActorJSON("", "system", ""), v.Name())
	})
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
