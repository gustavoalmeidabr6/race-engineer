// Command bakecenterline extracts a curated centerline (lap_distance_m,
// world x, world z) from `telemetry_hifreq` and writes it back into
// `workspace/tracks/<track_id>.json`. The Live Map / mock generator then use
// the centerline to render the real track shape before any lap is driven.
//
// Typical workflow: the operator drives one good lap at a track in real
// mode, then runs:
//
//	./workspace/bin/bakecenterline -track 7        # uses best valid lap
//	./workspace/bin/bakecenterline -track 7 -lap 12
//	./workspace/bin/bakecenterline -track 7 -session 11097389812345678901
//	./workspace/bin/bakecenterline -track 7 -points 250
//
// The tool is read-only against DuckDB and only mutates the JSON file. Safe
// to run while the main telemetry-core is live (opens the DB in read-only
// mode).
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "github.com/marcboeker/go-duckdb"
)

const defaultPoints = 250

func main() {
	track := flag.Int("track", -1, "F1 25 track id (required)")
	lapArg := flag.String("lap", "best", "lap to extract: `best`, `last`, or an integer lap number")
	sessionArg := flag.String("session", "", "session_uid to scope to; default: pick best across all sessions for the track")
	points := flag.Int("points", defaultPoints, "approximate number of centerline points to emit")
	tracksDir := flag.String("tracks", "workspace/tracks", "path to workspace/tracks directory")
	dbPath := flag.String("db", "workspace/telemetry.duckdb", "DuckDB file path")
	flag.Parse()

	if *track < 0 || *track > 127 {
		fmt.Fprintln(os.Stderr, "error: -track is required and must be in [0, 127]")
		os.Exit(2)
	}
	if *points < 30 || *points > 2000 {
		fmt.Fprintln(os.Stderr, "error: -points must be between 30 and 2000")
		os.Exit(2)
	}

	db, err := sql.Open("duckdb", *dbPath+"?access_mode=READ_ONLY")
	must(err, "open db")
	defer db.Close()
	must(db.Ping(), "ping db")

	// Resolve the session_uid + lap pair we'll pull from. session_uid is
	// optional; when missing we pick the best valid lap across every
	// session for the requested track.
	sessionUID, lapNum, lapTimeMs, trackLength, err := resolveSessionLap(db, *track, *sessionArg, *lapArg)
	must(err, "resolve lap")
	if sessionUID == 0 {
		fmt.Fprintln(os.Stderr, "error: no suitable lap found in DuckDB for the requested track")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "picked session=%d lap=%d lap_time_ms=%d track_length=%dm\n",
		sessionUID, lapNum, lapTimeMs, trackLength)

	// Pull a single sample per ~equal-spaced track_position bucket.
	pts, err := fetchCenterline(db, sessionUID, lapNum, *points, trackLength)
	must(err, "fetch centerline")
	if len(pts) < 30 {
		fmt.Fprintf(os.Stderr, "error: extracted only %d points — lap may be incomplete or paused\n", len(pts))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "extracted %d points\n", len(pts))

	// Write into workspace/tracks/<id>.json under "centerline". Preserves
	// every other field via a generic map[string]interface{} round-trip.
	if err := writeBack(*tracksDir, *track, pts); err != nil {
		must(err, "write back json")
	}
	fmt.Fprintf(os.Stderr, "✓ wrote centerline into %s/%d.json\n", *tracksDir, *track)
}

// ---------------------------------------------------------------------------
// Lap resolution.
// ---------------------------------------------------------------------------

// resolveSessionLap picks (session_uid, lap_num, lap_time_ms, track_length).
// `lapArg` is "best", "last", or an integer.
func resolveSessionLap(db *sql.DB, trackID int, sessionArg, lapArg string) (uint64, int, int, int, error) {
	// Map each session_uid to its track_id + track_length via session_data
	// timestamps (the timestamp ranges encode the session window). The
	// telemetry_hifreq + session_history tables have session_uid directly.
	sessionFilter := ""
	if sessionArg != "" {
		uid, err := strconv.ParseUint(sessionArg, 10, 64)
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("invalid -session: %w", err)
		}
		sessionFilter = fmt.Sprintf(" AND h.session_uid = %d", uid)
	}

	// Common subquery: lap_num + lap_time_ms + session_uid for every lap
	// that belongs to the requested track. session_history carries lap times
	// AND session_uid; we cross-check the lap's session against session_data
	// to confirm the track_id.
	q := fmt.Sprintf(`
WITH track_sessions AS (
  SELECT DISTINCT h.session_uid
  FROM telemetry_hifreq h
  JOIN session_data s
    ON s.timestamp BETWEEN
        (SELECT MIN(timestamp) FROM telemetry_hifreq WHERE session_uid = h.session_uid)
    AND (SELECT MAX(timestamp) FROM telemetry_hifreq WHERE session_uid = h.session_uid)
  WHERE s.track_id = %d
    %s
),
laps AS (
  SELECT sh.session_uid,
         sh.lap_num,
         sh.lap_time_in_ms,
         (SELECT MAX(track_length) FROM session_data
            WHERE timestamp BETWEEN
                (SELECT MIN(timestamp) FROM telemetry_hifreq WHERE session_uid = sh.session_uid)
            AND (SELECT MAX(timestamp) FROM telemetry_hifreq WHERE session_uid = sh.session_uid)
         ) AS track_length,
         (SELECT COUNT(*) FROM telemetry_hifreq
            WHERE session_uid = sh.session_uid AND lap = sh.lap_num) AS sample_count
  FROM session_history sh
  WHERE sh.session_uid IN (SELECT session_uid FROM track_sessions)
    AND sh.lap_valid = 1
    AND sh.lap_time_in_ms > 0
)
SELECT session_uid, lap_num, lap_time_in_ms, track_length
FROM laps
WHERE sample_count >= 100
`, trackID, sessionFilter)

	switch strings.ToLower(strings.TrimSpace(lapArg)) {
	case "", "best":
		q += "ORDER BY lap_time_in_ms ASC LIMIT 1"
	case "last":
		q += "ORDER BY session_uid DESC, lap_num DESC LIMIT 1"
	default:
		lapNum, err := strconv.Atoi(lapArg)
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("invalid -lap: %q (expected best|last|<int>)", lapArg)
		}
		q += fmt.Sprintf("AND lap_num = %d ORDER BY lap_time_in_ms ASC LIMIT 1", lapNum)
	}

	row := db.QueryRow(q)
	var uid uint64
	var lapNum, lapMs, trackLen int
	if err := row.Scan(&uid, &lapNum, &lapMs, &trackLen); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, 0, 0, nil
		}
		return 0, 0, 0, 0, err
	}
	return uid, lapNum, lapMs, trackLen, nil
}

// ---------------------------------------------------------------------------
// Centerline extraction.
// ---------------------------------------------------------------------------

type centerlinePoint struct {
	LapDistanceM float32 `json:"lap_distance_m"`
	X            float32 `json:"x"`
	Z            float32 `json:"z"`
}

// fetchCenterline pulls one sample per equal-spaced track_position bucket
// for the chosen lap. `targetPoints` controls bucket width — the resulting
// slice usually lands within ±5% of that count.
func fetchCenterline(db *sql.DB, sessionUID uint64, lapNum, targetPoints, trackLength int) ([]centerlinePoint, error) {
	if trackLength <= 0 {
		trackLength = 5000 // pessimistic fallback so the bucket math is bounded
	}
	bucketSize := float64(trackLength) / float64(targetPoints)
	q := fmt.Sprintf(`
WITH src AS (
  SELECT track_position, world_pos_x, world_pos_z,
         ROW_NUMBER() OVER (
           PARTITION BY CAST(track_position / %f AS INTEGER)
           ORDER BY ABS(track_position - (CAST(track_position / %f AS INTEGER) + 0.5) * %f)
         ) AS rn
  FROM telemetry_hifreq
  WHERE session_uid = %d
    AND lap = %d
    AND track_position IS NOT NULL
    AND world_pos_x IS NOT NULL
    AND world_pos_z IS NOT NULL
    AND pit_status = 0
)
SELECT track_position, world_pos_x, world_pos_z
FROM src
WHERE rn = 1
ORDER BY track_position`, bucketSize, bucketSize, bucketSize, sessionUID, lapNum)

	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pts := make([]centerlinePoint, 0, targetPoints)
	for rows.Next() {
		var d, x, z float64
		if err := rows.Scan(&d, &x, &z); err != nil {
			return nil, err
		}
		if !isFinite(d) || !isFinite(x) || !isFinite(z) {
			continue
		}
		pts = append(pts, centerlinePoint{
			LapDistanceM: float32(d),
			X:            float32(x),
			Z:            float32(z),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(pts, func(i, j int) bool {
		return pts[i].LapDistanceM < pts[j].LapDistanceM
	})
	return pts, nil
}

// ---------------------------------------------------------------------------
// JSON write-back.
// ---------------------------------------------------------------------------

// writeBack merges the centerline into workspace/tracks/<id>.json without
// disturbing any other curated field (corners, pit metadata, name, …).
// Pretty-prints with 2-space indentation for diff-friendliness.
func writeBack(dir string, trackID int, pts []centerlinePoint) error {
	path := filepath.Join(dir, fmt.Sprintf("%d.json", trackID))
	existing := map[string]interface{}{}
	if buf, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(buf, &existing) // ignore decode errors — we'll overwrite
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Marshal each point to compact one-line form for readability — the file
	// can have 250+ points and the operator should be able to scroll past
	// without scrolling for a thousand lines.
	compact := make([]json.RawMessage, len(pts))
	for i, p := range pts {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		compact[i] = b
	}
	existing["centerline"] = compact

	buf, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0o644)
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func must(err error, what string) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "error: %s: %v\n", what, err)
	os.Exit(1)
}
