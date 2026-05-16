package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/ingestion"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/storage"
)

// TestMockMode_EndToEnd verifies the full pipeline runs cleanly with no game
// attached: the mock generator drives the ingester, the atomic cache and
// DuckDB get populated, and the public REST handlers reflect that data.
func TestMockMode_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewStorage(filepath.Join(dir, "smoke.duckdb"), 5)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		UDPHost:    "127.0.0.1",
		UDPPort:    0,
		BatchSize:  5,
		SampleRate: 1,
		APIPort:    "0",
		DBPath:     "smoke.duckdb",
	}
	cfg.MockMode.Store(true)
	cfg.MockOverrides = models.MockOverrides{
		TireWearMultiplier: 1.0,
		FuelBurnMultiplier: 1.0,
	}
	cfg.SetRuntimeUDP("127.0.0.1", 0, "broadcast")

	ingester := ingestion.NewIngester(cfg, store)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	ingester.Start(ctx, &wg)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	deps := &Deps{
		Store:     store,
		Cfg:       cfg,
		StartedAt: time.Now(),
		PacketsRx: ingester.PacketsReceived,
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/api/telemetry/latest", latestTelemetryHandler(deps))
	app.Get("/api/settings", settingsHandler(deps))
	app.Get("/health", healthHandler(deps))

	// Wait for the cache to fill from mock packets. Mock CarTelemetry runs at
	// 20Hz, so this should land well under a second.
	if !waitFor(3*time.Second, func() bool {
		state := store.Cache().Load()
		return state != nil && state.Speed > 0
	}) {
		t.Fatalf("mock cache never populated within 3s")
	}

	// /api/telemetry/latest should return the populated state.
	code, body := doReq(t, app, "GET", "/api/telemetry/latest", nil)
	if code != 200 {
		t.Fatalf("/api/telemetry/latest status %d body %s", code, body)
	}
	var state models.RaceState
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode telemetry: %v\nbody: %s", err, body)
	}
	if state.Speed == 0 {
		t.Errorf("expected non-zero mock speed, got %d", state.Speed)
	}
	if state.EngineRPM == 0 {
		t.Errorf("expected non-zero RPM, got %d", state.EngineRPM)
	}

	// /api/settings should report mock_mode=true.
	code, body = doReq(t, app, "GET", "/api/settings", nil)
	if code != 200 {
		t.Fatalf("/api/settings status %d body %s", code, body)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if mock, _ := settings["mock_mode"].(bool); !mock {
		t.Errorf("expected mock_mode=true, got %v", settings["mock_mode"])
	}

	// /health should report packets_rx > 0 and duckdb_ok.
	code, body = doReq(t, app, "GET", "/health", nil)
	if code != 200 {
		t.Fatalf("/health status %d", code)
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if rx, _ := health["packets_rx"].(float64); rx == 0 {
		t.Errorf("expected packets_rx > 0 in mock mode, got %v", health["packets_rx"])
	}
	if ok, _ := health["duckdb_ok"].(bool); !ok {
		t.Errorf("duckdb_ok should be true, got %v", health["duckdb_ok"])
	}
}
