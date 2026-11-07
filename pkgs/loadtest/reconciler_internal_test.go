package loadtest

// Reconciler tests live in the internal test package because they exercise
// the unexported tick() directly. They build their own DB (the shared
// testutil.NewServer composition cannot be imported here without an import
// cycle: server → loadtest). Rows are inserted directly instead of going
// through the HTTP create handler.

import (
	"context"
	"os"
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
	return runner.Status{State: f.states[ref], Missing: f.missing[ref]}, nil
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
