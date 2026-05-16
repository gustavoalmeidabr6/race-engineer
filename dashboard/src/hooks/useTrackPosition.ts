import { useEffect, useState } from 'react';
import { API_BASE } from '../lib/constants';
import { useTelemetryStream } from '../context/WebSocketContext';
import type { TrackGeometryView, TrackPositionResponse } from '../types/trackPosition';

/**
 * Composes the LiveMap data feed:
 *   - per-tick player + grid + headline from the /ws `track_position`
 *     message (pushed at WS_PUSH_RATE — 10Hz default)
 *   - static track geometry (corners, sector_starts, length) fetched ONCE
 *     via REST when the player's track_id first resolves
 *
 * Returns the same `TrackPositionResponse` shape the REST endpoint used to
 * return so consumers (LiveMap, TrackMapSVG, Leaderboard) are unchanged.
 *
 * `intervalMs` is kept as a no-op parameter for backwards-compat with the
 * old polling signature — the WS push rate is server-controlled.
 */
export function useTrackPosition(_intervalMs?: number) {
  const { state, trackPos } = useTelemetryStream();
  const [geometry, setGeometry] = useState<TrackGeometryView | null>(null);
  const [geometryTrackId, setGeometryTrackId] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);

  const trackId = state?.track_id ?? null;

  useEffect(() => {
    // Re-fetch geometry once per track. The REST handler is in-memory cheap
    // and only the static block is read off the response.
    if (trackId === null) return;
    if (geometryTrackId === trackId && geometry) return;

    let active = true;
    const ctrl = new AbortController();
    (async () => {
      try {
        const res = await fetch(`${API_BASE}/api/state/track_position`, { signal: ctrl.signal });
        if (!active) return;
        if (res.status === 503) {
          setError('no telemetry yet');
          return;
        }
        if (!res.ok) {
          setError(`${res.status} ${res.statusText}`);
          return;
        }
        const json = (await res.json()) as TrackPositionResponse;
        if (!active) return;
        setGeometry(json.track);
        setGeometryTrackId(json.track.track_id);
        setError(null);
      } catch (e) {
        if (!active) return;
        if (e instanceof DOMException && e.name === 'AbortError') return;
        setError(e instanceof Error ? e.message : String(e));
      }
    })();

    return () => {
      active = false;
      ctrl.abort();
    };
  }, [trackId, geometryTrackId, geometry]);

  const data: TrackPositionResponse | null =
    geometry && trackPos
      ? {
          headline: trackPos.headline,
          track: geometry,
          me: trackPos.me,
          grid: trackPos.grid,
        }
      : null;

  return { data, error };
}
