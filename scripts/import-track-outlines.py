#!/usr/bin/env python3
"""
import-track-outlines.py — bake centerlines for every F1 25 track from the
bacinger/f1-circuits open data set (MIT-licensed).

For each F1 25 track id we map to a bacinger circuit slug, fetch its
GeoJSON LineString, project lon/lat to local-meter Cartesian via
equirectangular projection around the polyline's centroid, resample to
~250 evenly-spaced points along cumulative arc length, and merge a
`centerline` field into workspace/tracks/<id>.json (preserving every
existing field).

We deliberately skip track 7 (Silverstone) — its centerline is already
calibrated to F1 game world coordinates and shouldn't be replaced with
external coords.

External coords don't share the F1 game's (x, y, z) frame, but the
dashboard tolerates that:

  * `computeBBox` auto-fits the SVG viewBox to whatever (x, z) it sees.
  * Cars snap to the outline by `lap_distance_m`, which is invariant.
  * When a real lap completes, `fetchOutline()` returns hi-freq data in
    true game coords and the shape switches over (one-time visual snap,
    acceptable; happens at most once per track per install).

Usage: python3 scripts/import-track-outlines.py
"""

from __future__ import annotations

import json
import math
import sys
import urllib.request
from pathlib import Path

# ── Mapping: F1 25 track id → (bacinger slug, friendly name) ─────────────
# Track 7 (Silverstone) is intentionally excluded; its workspace JSON
# already ships a centerline calibrated to F1 game world coords.
TRACK_MAP: dict[int, tuple[str, str]] = {
    0:  ("au-1953", "Melbourne"),
    3:  ("bh-2002", "Bahrain"),
    4:  ("es-1991", "Catalunya"),
    5:  ("mc-1929", "Monaco"),
    9:  ("hu-1986", "Hungaroring"),
    10: ("be-1925", "Spa"),
    11: ("it-1922", "Monza"),
    13: ("jp-1962", "Suzuka"),
    14: ("ae-2009", "Abu Dhabi"),
    15: ("us-2012", "Austin"),
    17: ("at-1969", "Red Bull Ring"),
    19: ("mx-1962", "Mexico City"),
    26: ("nl-1948", "Zandvoort"),
    27: ("it-1953", "Imola"),
    29: ("sa-2021", "Jeddah"),
    30: ("us-2022", "Miami"),
}

REPO_BASE = "https://raw.githubusercontent.com/bacinger/f1-circuits/master/circuits"
TARGET_POINTS = 250

REPO_ROOT = Path(__file__).resolve().parent.parent
TRACKS_DIR = REPO_ROOT / "workspace" / "tracks"


def haversine_m(lat1: float, lon1: float, lat2: float, lon2: float) -> float:
    """Great-circle distance between two lon/lat points in metres."""
    R = 6371000.0
    p1 = math.radians(lat1)
    p2 = math.radians(lat2)
    dphi = math.radians(lat2 - lat1)
    dl = math.radians(lon2 - lon1)
    a = math.sin(dphi / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dl / 2) ** 2
    return 2 * R * math.asin(math.sqrt(a))


def project_local(coords: list[list[float]]) -> list[tuple[float, float]]:
    """Equirectangular projection around the polyline's centroid.

    Returns (x, z) metres where +x is east and +z is north (negate of
    delta-lat per the plan, matching the F1 world convention used by the
    dashboard).
    """
    lat_c = sum(c[1] for c in coords) / len(coords)
    lon_c = sum(c[0] for c in coords) / len(coords)
    cos_lat_c = math.cos(math.radians(lat_c))
    out: list[tuple[float, float]] = []
    for lon, lat in coords:
        x = (lon - lon_c) * 111320.0 * cos_lat_c
        z = -(lat - lat_c) * 110540.0
        out.append((x, z))
    return out


def cumulative_distances(coords: list[list[float]]) -> list[float]:
    """Cumulative haversine distance along the polyline, parallel to coords."""
    out = [0.0]
    for i in range(1, len(coords)):
        d = haversine_m(coords[i - 1][1], coords[i - 1][0], coords[i][1], coords[i][0])
        out.append(out[-1] + d)
    return out


def resample(
    points: list[tuple[float, float]],
    distances: list[float],
    n: int,
) -> list[tuple[float, float, float]]:
    """Evenly-space n samples along the cumulative arc-length axis.

    Returns (lap_distance_m, x, z) tuples.
    """
    total = distances[-1]
    if total <= 0 or n < 2:
        return []
    out: list[tuple[float, float, float]] = []
    j = 0
    for i in range(n):
        t = (i / (n - 1)) * total
        # Advance j so distances[j] <= t <= distances[j+1].
        while j < len(distances) - 2 and distances[j + 1] < t:
            j += 1
        a, b = distances[j], distances[j + 1]
        span = b - a
        f = 0.0 if span <= 0 else (t - a) / span
        px = points[j][0] + (points[j + 1][0] - points[j][0]) * f
        pz = points[j][1] + (points[j + 1][1] - points[j][1]) * f
        out.append((t, px, pz))
    return out


def fetch_geojson(slug: str) -> dict:
    url = f"{REPO_BASE}/{slug}.geojson"
    print(f"  fetching {url}")
    with urllib.request.urlopen(url, timeout=30) as r:
        return json.loads(r.read())


def extract_linestring_coords(geojson: dict) -> list[list[float]]:
    """Pull the first LineString feature's coordinates (lon, lat pairs)."""
    for feat in geojson.get("features", []):
        geom = feat.get("geometry") or {}
        if geom.get("type") == "LineString":
            return geom["coordinates"]
    raise RuntimeError("no LineString feature in GeoJSON")


def import_one(track_id: int, slug: str, name: str):
    target_path = TRACKS_DIR / f"{track_id}.json"
    if not target_path.exists():
        print(f"  skip: {target_path} does not exist")
        return None
    geojson = fetch_geojson(slug)
    coords = extract_linestring_coords(geojson)
    if len(coords) < 4:
        print(f"  skip: too few points ({len(coords)})")
        return None
    distances = cumulative_distances(coords)
    points = project_local(coords)
    samples = resample(points, distances, TARGET_POINTS)

    track = json.loads(target_path.read_text())
    track["centerline"] = [
        {"lap_distance_m": round(t, 2), "x": round(x, 3), "z": round(z, 3)}
        for (t, x, z) in samples
    ]
    target_path.write_text(json.dumps(track, indent=2) + "\n")

    perimeter = distances[-1]
    declared = float(track.get("length_m") or 0.0)
    err = (perimeter - declared) / declared * 100.0 if declared > 0 else float("nan")
    print(
        f"  {name:<16}  pts={len(samples):>3}  perim={perimeter:>5.0f}m  "
        f"decl={declared:>5.0f}m  err={err:+.1f}%"
    )
    return (name, len(samples), perimeter, declared, err)


def main() -> int:
    if not TRACKS_DIR.is_dir():
        print(f"error: {TRACKS_DIR} not found", file=sys.stderr)
        return 1

    print("Importing track centerlines from bacinger/f1-circuits …")
    rows = []
    for tid, (slug, name) in sorted(TRACK_MAP.items()):
        print(f"track {tid} ({slug})")
        try:
            row = import_one(tid, slug, name)
            if row:
                rows.append((tid, *row))
        except Exception as e:  # noqa: BLE001 — script-level, want full context
            print(f"  ERROR: {e}", file=sys.stderr)

    print()
    print("Summary:")
    print(f"{'id':>3}  {'name':<16} {'pts':>4}  {'perim_m':>8} {'decl_m':>8} {'err_%':>6}")
    for tid, name, pts, perim, decl, err in rows:
        print(f"{tid:>3}  {name:<16} {pts:>4}  {perim:>8.0f} {decl:>8.0f} {err:>+6.1f}")
    print()
    print(f"Imported {len(rows)} / {len(TRACK_MAP)} tracks.")
    return 0 if len(rows) == len(TRACK_MAP) else 2


if __name__ == "__main__":
    sys.exit(main())
