# Track Geometry Data

Each `<track_id>.json` matches a F1 25 track id (see CLAUDE.md → Track IDs).

## Schema

```json
{
  "name": "Silverstone",
  "length_m": 5891,
  "_note": "Optional comment about source / accuracy.",
  "corners": [
    {"id": "T1", "name": "Abbey", "lap_distance_m": 280, "type": "fast"}
  ]
}
```

`type` is `slow` (≤120 km/h apex), `medium` (~150–220), `fast` (≥220), or `kink`
(no real braking).

## Accuracy

Distances are hand-curated approximations from public circuit guides and
layout references — accurate to roughly ±50–100m on most tracks. The
reminder engine uses a 1.5s × speed lookahead (~125m at 300 km/h), so this
precision is good enough for "brake earlier" coaching cues but not for
sub-50m apex calls.

To refine a track from real telemetry: race a clean lap, then in DuckDB
correlate `motion_data.world_position_*` with `lap_data.lap_distance` at
the moments steering peaks for each corner.

## Adding a new track

Drop in a new `<track_id>.json`; the registry loads on server boot. No
recompile needed.

## Coverage

Tracks NOT mapped here fall back gracefully — `get_track_position` still
returns sector boundaries (from F1 25 SessionPacket) and per-car
lap-distance, just no corner names. The agent can still set
`set_corner_reminder` with `trigger="lap_distance"` on any track.
