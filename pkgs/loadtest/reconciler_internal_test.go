package loadtest

// Reconciler tests live in the internal test package because they exercise
// the unexported tick() directly. They build their own DB (the shared
// testutil.NewServer composition cannot be imported here without an import
// cycle: server → loadtest). Rows are inserted directly instead of going
// through the HTTP create handler.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platform "github.com/hungpham/loadtest-platform"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"

	"gorm.io/gorm"
)

// fakeRunner records calls without running k6.
type fakeRunner struct {
	states  map[string]string
	missing map[string]bool
	// summary/summaryErr are attached to every Status call (plans/45).
	summary    []byte
	summaryErr string
}

func (f *fakeRunner) Mode() string { return "local" }

func (f *fakeRunner) Provision(_ context.Context, _ runner.Spec) (string, error) {
	ref := "fake-" + time.Now().Format(time.RFC3339Nano)
	f.states = map[string]string{}
	return ref, nil
}
func (f *fakeRunner) Start(_ context.Context, _ string, _ runner.Spec) error { return nil }
func (f *fakeRunner) Stop(_ context.Context, _ string) error                 { return nil }
func (f *fakeRunner) Status(_ context.Context, ref string) (runner.Status, error) {
	return runner.Status{
		State: f.states[ref], Missing: f.missing[ref],
		Summary: f.summary, SummaryErr: f.summaryErr,
	}, nil
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable"
	}
	db, err := store.New(context.Background(), dsn)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI requires postgres: %v", err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := store.Migrate(context.Background(), dsn, platform.Migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// provisioningFixture inserts tenant/user/namespace/script plus a
// PROVISIONING load test (runner_ref set).
func provisioningFixture(t *testing.T, db *gorm.DB, ref string) store.LoadTest {
	t.Helper()
	tenant := store.Tenant{Name: "L", Slug: "ltr" + time.Now().Format("150405.000000000"), Status: "ACTIVE"}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	user := store.User{TenantID: tenant.ID, Email: "ltr-" + tenant.ID + "@x.co", DisplayName: "L"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	ns := store.Namespace{
		TenantID: tenant.ID, OwnerUserID: user.ID, Name: "ns-" + tenant.ID,
		Status: "READY", ReadyForLoadtest: true, Version: 1, CleanupPolicy: []byte("{}"),
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatal(err)
	}
	sc := store.Script{
		NamespaceID: ns.ID, Name: "s-" + tenant.ID, ContentRef: ns.ID + "/s.js",
		ContentHash: "abc", SizeBytes: 10, Status: "VALIDATED", Version: 1,
	}
	if err := db.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}
	lt := store.LoadTest{
		NamespaceID: ns.ID, ScriptID: sc.ID, CreatedBy: user.ID,
		Name: "ltr-" + time.Now().Format(time.RFC3339Nano), Status: "PROVISIONING",
		RunnerType: "local", Version: 1, RunnerRef: &ref,
		LoadConfig: []byte(`{"vusers":1,"duration":"1m"}`), Targets: []byte(`[]`),
	}
	if err := db.Create(&lt).Error; err != nil {
		t.Fatal(err)
	}
	return lt
}

func TestReconcilerAdvancesStates(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}}
	ref, _ := fr.Provision(context.Background(), runner.Spec{})
	lt := provisioningFixture(t, db, ref)

	rc := Reconciler{DB: db, Runner: fr, Every: time.Hour, Now: time.Now}

	// PROVISIONING + runner RUNNING → RUNNING
	fr.states[ref] = "RUNNING"
	rc.tick(context.Background())
	var after store.LoadTest
	_ = db.First(&after, "id = ?", lt.ID)
	if after.Status != "RUNNING" {
		t.Fatalf("after provisioning tick = %s, want RUNNING", after.Status)
	}

	// RUNNING + runner COMPLETED → COMPLETED
	fr.states[ref] = "COMPLETED"
	rc.tick(context.Background())
	_ = db.First(&after, "id = ?", lt.ID)
	if after.Status != "COMPLETED" {
		t.Fatalf("after completion tick = %s, want COMPLETED", after.Status)
	}
	if after.FinishedAt == nil {
		t.Error("finished_at must be set")
	}

	// event trail: RUNNING + COMPLETED
	var count int64
	_ = db.Model(&store.LoadTestEvent{}).Where("object_id = ?", lt.ID).Count(&count).Error
	if count < 2 {
		t.Errorf("event rows = %d, want >= 2 (RUNNING, COMPLETED)", count)
	}
}

func TestReconcilerTimeout(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}}
	ref, _ := fr.Provision(context.Background(), runner.Spec{})
	lt := provisioningFixture(t, db, ref)

	past := time.Now().Add(-time.Hour)
	if err := db.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
		Updates(map[string]any{"status": "RUNNING", "timeout_at": &past, "started_at": &past}).Error; err != nil {
		t.Fatal(err)
	}
	rc := Reconciler{DB: db, Runner: &fakeRunner{states: map[string]string{}}, Every: time.Hour, Now: time.Now}
	rc.tick(context.Background())
	var after store.LoadTest
	_ = db.First(&after, "id = ?", lt.ID)
	if after.Status != "TIMEOUT_CLEANED" {
		t.Errorf("status after timeout reap = %s, want TIMEOUT_CLEANED", after.Status)
	}
}

// TestReconcilerRunnerLostRun: when the runner no longer knows the ref
// (Missing=true — e.g. controller restart wiped its in-process proc table),
// PROVISIONING and RUNNING rows must land in FAILED, never hang active.
func TestReconcilerRunnerLostRun(t *testing.T) {
	for _, status := range []string{"PROVISIONING", "RUNNING"} {
		db := testDB(t)
		fr := &fakeRunner{missing: map[string]bool{}}
		ref, _ := fr.Provision(context.Background(), runner.Spec{})
		lt := provisioningFixture(t, db, ref)
		if status != "PROVISIONING" {
			if err := db.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
				Update("status", "RUNNING").Error; err != nil {
				t.Fatal(err)
			}
		}
		// ref unknown to the runner (STOPPED + Missing)
		fr.missing[ref] = true

		rc := Reconciler{DB: db, Runner: fr, Every: time.Hour, Now: time.Now}
		rc.tick(context.Background())

		var after store.LoadTest
		_ = db.First(&after, "id = ?", lt.ID)
		if after.Status != "FAILED" {
			t.Errorf("status=%s runner=Missing: want FAILED", after.Status)
		}
		var ev store.LoadTestEvent
		if err := db.Where("object_id = ?", lt.ID).Order("id DESC").First(&ev).Error; err != nil ||
			ev.Reason == nil || !strings.Contains(*ev.Reason, "runner lost the run") {
			t.Errorf("event reason = (%+v, %v), want 'runner lost the run'", ev.Reason, err)
		}
	}
}

// --- results collection (plans/45) ----------------------------------------

func readSummaryFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "runner", "testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return raw
}

// runToCompletion drives a PROVISIONING run to COMPLETED with the given status
// payload from the fake runner.
func runToCompletion(t *testing.T, db *gorm.DB, fr *fakeRunner) (store.LoadTest, Reconciler) {
	t.Helper()
	ref, _ := fr.Provision(context.Background(), runner.Spec{})
	lt := provisioningFixture(t, db, ref)
	rc := Reconciler{DB: db, Runner: fr, Every: time.Hour, Now: time.Now}
	fr.states[ref] = "RUNNING"
	rc.tick(context.Background())
	fr.states[ref] = "COMPLETED"
	rc.tick(context.Background())
	var after store.LoadTest
	if err := db.First(&after, "id = ?", lt.ID).Error; err != nil {
		t.Fatal(err)
	}
	return after, rc
}

// TestReconcilerStoresSummary — a completed run persists the parsed summary in
// the same transition that finishes it.
func TestReconcilerStoresSummary(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}, summary: readSummaryFixture(t, "pass.txt")}
	after, _ := runToCompletion(t, db, fr)

	if after.Status != "COMPLETED" {
		t.Fatalf("status = %s", after.Status)
	}
	if after.ResultCollectedAt == nil {
		t.Fatal("result_collected_at is NULL after a completed run with a summary")
	}
	if after.ResultParseError != nil {
		t.Errorf("result_parse_error = %q", *after.ResultParseError)
	}
	if after.ThresholdsFailed == nil || *after.ThresholdsFailed {
		t.Errorf("thresholds_failed = %v, want false", after.ThresholdsFailed)
	}
	if len(after.ResultSummary) == 0 {
		t.Fatal("result_summary is empty")
	}
	var sum runner.Summary
	if err := json.Unmarshal(after.ResultSummary, &sum); err != nil {
		t.Fatalf("result_summary is not a Summary: %v", err)
	}
	if sum.HTTPReqs != 2 || sum.P95 == 0 {
		t.Errorf("stored summary = %+v", sum)
	}
}

// TestReconcilerStoresFailedThresholds — a threshold violation is the result a
// consumer must be able to act on.
func TestReconcilerStoresFailedThresholds(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}, summary: readSummaryFixture(t, "fail.txt")}
	after, _ := runToCompletion(t, db, fr)
	if after.ThresholdsFailed == nil || !*after.ThresholdsFailed {
		t.Fatalf("thresholds_failed = %v, want true", after.ThresholdsFailed)
	}
	if after.ResultCollectedAt == nil {
		t.Error("result_collected_at is NULL")
	}
}

// TestReconcilerCollectionIsIdempotent — a retried/duplicated finish must never
// overwrite the first summary, nor move result_collected_at.
func TestReconcilerCollectionIsIdempotent(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}, summary: readSummaryFixture(t, "pass.txt")}
	after, rc := runToCompletion(t, db, fr)

	first := *after.ResultCollectedAt
	firstSummary := string(after.ResultSummary)

	// Second finish with a different (empty) summary: the IS NULL guard wins.
	after.ResultSummary = nil
	rc.finish(context.Background(), &after, runner.Status{SummaryErr: "pod gone"}, "COMPLETED",
		store.ActorJSON("", "system", ""), "retry")
	var again store.LoadTest
	_ = db.First(&again, "id = ?", after.ID)
	if string(again.ResultSummary) != firstSummary {
		t.Error("a retried finish overwrote the stored summary")
	}
	if again.ResultCollectedAt == nil || !again.ResultCollectedAt.Equal(first) {
		t.Errorf("result_collected_at moved: %v → %v", first, again.ResultCollectedAt)
	}
	if again.ResultParseError != nil {
		t.Errorf("result_parse_error = %q after a retry", *again.ResultParseError)
	}
}

// TestReconcilerStoresRetrievalError — the pod is gone but the run still
// finished: terminal status kept, reason recorded, API can distinguish
// "collected nothing" from "never collected".
func TestReconcilerStoresRetrievalError(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}, summaryErr: "runner pod not found (already deleted?)"}
	after, _ := runToCompletion(t, db, fr)
	if after.Status != "COMPLETED" {
		t.Fatalf("status = %s, want COMPLETED (a missing log is not a failed run)", after.Status)
	}
	if after.ResultCollectedAt == nil {
		t.Fatal("result_collected_at is NULL for a retrieval failure")
	}
	if after.ResultParseError == nil || !strings.Contains(*after.ResultParseError, "pod not found") {
		t.Errorf("result_parse_error = %v", after.ResultParseError)
	}
	if len(after.ResultSummary) != 0 {
		t.Errorf("result_summary = %s, want empty", after.ResultSummary)
	}
}

// TestReconcilerTimeoutRecordsNoSummary — a killed run has no trustworthy
// result; say so instead of leaving the row looking uncollected.
func TestReconcilerTimeoutRecordsNoSummary(t *testing.T) {
	db := testDB(t)
	fr := &fakeRunner{states: map[string]string{}}
	ref, _ := fr.Provision(context.Background(), runner.Spec{})
	lt := provisioningFixture(t, db, ref)
	past := time.Now().Add(-time.Hour)
	if err := db.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
		Updates(map[string]any{"status": "RUNNING", "timeout_at": &past, "started_at": &past}).Error; err != nil {
		t.Fatal(err)
	}
	rc := Reconciler{DB: db, Runner: fr, Every: time.Hour, Now: time.Now}
	rc.tick(context.Background())

	var after store.LoadTest
	_ = db.First(&after, "id = ?", lt.ID)
	if after.Status != "TIMEOUT_CLEANED" {
		t.Fatalf("status = %s", after.Status)
	}
	if after.ResultCollectedAt == nil {
		t.Error("result_collected_at is NULL after a timeout")
	}
	if after.ResultParseError == nil || !strings.Contains(*after.ResultParseError, "timed out") {
		t.Errorf("result_parse_error = %v, want a timeout explanation", after.ResultParseError)
	}
}
