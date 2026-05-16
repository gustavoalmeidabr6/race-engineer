import { useEffect, useState } from 'react';
import { API_BASE } from '../lib/constants';
import type { TrackOutlineResponse } from '../types/trackPosition';

/**
 * Fetches the recorded racing line once, then re-fetches when the active
 * session_uid changes. Stale outlines stay rendered while a new one loads
 * (no flicker on session rollover).
 *
 * Module-scope cache keyed by (track_id, session_uid). The outline is
 * effectively immutable per session, so any subsequent mount or page
 * navigation re-uses it without an HTTP round-trip.
 */
const outlineCache = new Map<string, TrackOutlineResponse>();

function cacheKey(trackId: number, sessionUid: string) {
  return `${trackId}::${sessionUid}`;
}

/**
 * Outlines below this many points are considered "still warming up" — the
 * hook will keep retrying until a meaningful trace exists. Mirrors the
 * MIN_USABLE_OUTLINE constant in TrackMapSVG.
 */
const MIN_USABLE_POINTS = 20;
const RETRY_MS = 8000;

export function useTrackOutline(trackId: number | undefined, sessionUid: string | undefined) {
  const [data, setData] = useState<TrackOutlineResponse | null>(() => {
    if (trackId == null || !sessionUid) return null;
    return outlineCache.get(cacheKey(trackId, sessionUid)) ?? null;
  });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (trackId == null || !sessionUid) {
      setData(null);
      return;
    }
    const key = cacheKey(trackId, sessionUid);
    const ctrl = new AbortController();
    let timer: ReturnType<typeof setTimeout> | null = null;

    const fetchOnce = async () => {
      setLoading(true);
      try {
        const res = await fetch(`${API_BASE}/api/track/outline`, { signal: ctrl.signal });
        if (!res.ok) {
          setError(`${res.status} ${res.statusText}`);
          return;
        }
        const json = (await res.json()) as TrackOutlineResponse;
        outlineCache.set(key, json);
        setData(json);
        setError(null);
        // Re-fetch on a slow loop until the outline is rich enough to
        // render. Once we have a usable trace, stop polling — the line is
        // effectively immutable for the rest of the session.
        if (json.points.length < MIN_USABLE_POINTS) {
          timer = setTimeout(fetchOnce, RETRY_MS);
        }
      } catch (e) {
        if (e instanceof DOMException && e.name === 'AbortError') return;
        setError(e instanceof Error ? e.message : String(e));
        timer = setTimeout(fetchOnce, RETRY_MS);
      } finally {
        setLoading(false);
      }
    };

    const cached = outlineCache.get(key);
    if (cached && cached.points.length >= MIN_USABLE_POINTS) {
      setData(cached);
    } else {
      fetchOnce();
    }

    return () => {
      ctrl.abort();
      if (timer) clearTimeout(timer);
    };
  }, [trackId, sessionUid]);

  return { data, loading, error };
}
