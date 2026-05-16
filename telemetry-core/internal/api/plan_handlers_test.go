package api

import (
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/plan"
)

func newPlanApp(t *testing.T) (*fiber.App, *plan.Store) {
	t.Helper()
	store, err := plan.NewStore(plan.Config{Dir: t.TempDir()}, func() uint64 { return 1 })
	if err != nil {
		t.Fatalf("plan.NewStore: %v", err)
	}
	deps := &Deps{Plan: store}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/api/plan/current", planCurrentHandler(deps))
	app.Post("/api/plan/entries", planAddHandler(deps))
	app.Patch("/api/plan/entries/:id", planUpdateHandler(deps))
	app.Delete("/api/plan/entries/:id", planCancelHandler(deps))
	return app, store
}

// TestPlanAdd_Returns201WithStoredEntry — POST /api/plan/entries assigns
// an id, default status, and persists.
func TestPlanAdd_Returns201WithStoredEntry(t *testing.T) {
	app, store := newPlanApp(t)
	body := []byte(`{"kind":"pit_stop","trigger":{"type":"lap","lap":14},"notes":"hard tyres","payload":{"compound":"hard"}}`)
	code, raw := doReq(t, app, "POST", "/api/plan/entries", body)
	if code != 201 {
		t.Fatalf("expected 201, got %d: %s", code, raw)
	}
	var got plan.PlanEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if got.ID == "" || got.ID[:3] != "pe_" {
		t.Errorf("expected pe_ prefix id, got %q", got.ID)
	}
	if got.Status != plan.StatusPending {
		t.Errorf("expected status=pending, got %q", got.Status)
	}
	if got.CreatedBy != "api" {
		t.Errorf("expected created_by=api, got %q", got.CreatedBy)
	}
	// Same entry is reachable via GET /api/plan/current.
	code, raw = doReq(t, app, "GET", "/api/plan/current", nil)
	if code != 200 {
		t.Fatalf("current GET %d: %s", code, raw)
	}
	var current plan.Plan
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatalf("parse current: %v", err)
	}
	if len(current.Entries) != 1 || current.Entries[0].ID != got.ID {
		t.Errorf("current plan should hold the new entry, got %+v", current)
	}
	_ = store
}

// TestPlanAdd_ValidationFailure — invalid trigger yields 400.
func TestPlanAdd_ValidationFailure(t *testing.T) {
	app, _ := newPlanApp(t)
	body := []byte(`{"kind":"pit_stop","trigger":{"type":"lap","lap":0}}`)
	code, raw := doReq(t, app, "POST", "/api/plan/entries", body)
	if code != 400 {
		t.Fatalf("expected 400 on bad trigger, got %d: %s", code, raw)
	}
}

// TestPlanUpdate_PatchesFields — PATCH only touches the fields included
// in the body.
func TestPlanUpdate_PatchesFields(t *testing.T) {
	app, store := newPlanApp(t)
	entry, _ := store.Add(plan.PlanEntry{
		Kind:    plan.KindPitStop,
		Trigger: plan.Trigger{Type: plan.TriggerLap, Lap: 14},
		Notes:   "original",
	}, "test")

	body := []byte(`{"status":"done","notes":"completed"}`)
	code, raw := doReq(t, app, "PATCH", "/api/plan/entries/"+entry.ID, body)
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, raw)
	}
	var got plan.PlanEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Status != plan.StatusDone {
		t.Errorf("expected status=done, got %q", got.Status)
	}
	if got.Notes != "completed" {
		t.Errorf("expected notes=completed, got %q", got.Notes)
	}
	// Untouched fields preserved.
	if got.Kind != plan.KindPitStop {
		t.Errorf("kind should be preserved, got %q", got.Kind)
	}
}

// TestPlanUpdate_404 — unknown id → 404.
func TestPlanUpdate_404(t *testing.T) {
	app, _ := newPlanApp(t)
	code, _ := doReq(t, app, "PATCH", "/api/plan/entries/pe_missing", []byte(`{"status":"done"}`))
	if code != 404 {
		t.Errorf("expected 404, got %d", code)
	}
}

// TestPlanCancel_SetsCancelledStatus — DELETE marks the entry as
// cancelled but keeps it in the plan history.
func TestPlanCancel_SetsCancelledStatus(t *testing.T) {
	app, store := newPlanApp(t)
	entry, _ := store.Add(plan.PlanEntry{
		Kind:    plan.KindPitStop,
		Trigger: plan.Trigger{Type: plan.TriggerLap, Lap: 14},
	}, "test")
	code, raw := doReq(t, app, "DELETE", "/api/plan/entries/"+entry.ID+"?reason=weather", nil)
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, raw)
	}
	var got plan.PlanEntry
	_ = json.Unmarshal(raw, &got)
	if got.Status != plan.StatusCancelled {
		t.Errorf("expected status=cancelled, got %q", got.Status)
	}
	if got.Notes == "" {
		t.Errorf("expected reason appended to notes")
	}
}

// TestPlanCurrent_NoStore503 — surface 503 when the store wasn't wired.
func TestPlanCurrent_NoStore503(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/api/plan/current", planCurrentHandler(&Deps{}))
	code, _ := doReq(t, app, "GET", "/api/plan/current", nil)
	if code != 503 {
		t.Errorf("expected 503 when plan store nil, got %d", code)
	}
}
