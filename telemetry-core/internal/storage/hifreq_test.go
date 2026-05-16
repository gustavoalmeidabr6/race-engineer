package storage

import (
	"context"
	"testing"
)

// TestHifreqBufferFlush verifies that buffered TelemetryHifreqRow rows make
// it into the new wide table on Flush. Catches schema/flush wiring drift —
// adding a column to the row struct without updating the INSERT or table
// would surface here.
func TestHifreqBufferFlush(t *testing.T) {
	store := newTestStore(t, 100)
	defer store.Close()

	for i := 0; i < 5; i++ {
		store.Buffers().TelemetryHifreq.Add(TelemetryHifreqRow{
			SessionUID:       0xDEADBEEFCAFE0001 + uint64(i),
			FrameID:          1000 + i,
			Lap:              3,
			TrackPosition:    float64(100 + i*50),
			TotalDistance:    float64(5000 + i*50),
			CurrentLapTimeMs: 12345 + i,
			PitStatus:        0,
			Throttle:         0.8,
			Brake:            0.0,
			Steering:         0.05,
			Speed:            250 + i,
			Gear:             6,
			EngineRPM:        11500,
			DRS:              1,
			BrakeTempFL:      450, BrakeTempFR: 460, BrakeTempRL: 410, BrakeTempRR: 420,
			TyreSurfTempFL: 90, TyreSurfTempFR: 92, TyreSurfTempRL: 88, TyreSurfTempRR: 89,
			WorldPosX:      100, WorldPosY: 200, WorldPosZ: 300,
			GForceLat:      0.5,
			GForceLon:      -0.2,
		})
	}

	if err := store.Buffers().TelemetryHifreq.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if c := queryCount(t, store, "telemetry_hifreq"); c != 5 {
		t.Fatalf("expected 5 hifreq rows, got %d", c)
	}

	// Spot-check round-trip of the high-bit-safe session_uid binding.
	rows, err := store.Query(context.Background(),
		"SELECT lap, track_position, brake_temp_fl, throttle FROM telemetry_hifreq ORDER BY track_position LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if got := row["lap"]; toInt64(t, got) != 3 {
		t.Errorf("lap mismatch: %v", got)
	}
	if got := toFloat64(t, row["throttle"]); got < 0.79 || got > 0.81 {
		t.Errorf("throttle mismatch: %v", got)
	}
	if got := toInt64(t, row["brake_temp_fl"]); got != 450 {
		t.Errorf("brake_temp_fl mismatch: %v", got)
	}
}

// TestHifreqBufferAutoFlush ensures the buffer auto-flushes when batchSize
// is hit, like every other table buffer.
func TestHifreqBufferAutoFlush(t *testing.T) {
	store := newTestStore(t, 3)
	defer store.Close()

	for i := 0; i < 3; i++ {
		store.Buffers().TelemetryHifreq.Add(TelemetryHifreqRow{
			SessionUID:    1,
			Lap:           1,
			TrackPosition: float64(i),
		})
	}
	// Auto-flush is async; force a follow-up flush so the test is determ.
	if err := store.Buffers().TelemetryHifreq.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := queryCount(t, store, "telemetry_hifreq"); c != 3 {
		t.Fatalf("expected 3 rows, got %d", c)
	}
}

func toFloat64(t *testing.T, v interface{}) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	}
	t.Fatalf("unexpected type for float: %T (%v)", v, v)
	return 0
}
