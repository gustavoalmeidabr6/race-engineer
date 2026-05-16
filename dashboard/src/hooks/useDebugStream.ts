import { useCallback, useEffect, useRef, useState } from 'react';
import { API_BASE } from '../lib/constants';

// DebugEvent mirrors transcript.Event on the server. Kept loose-typed
// because new kinds will land in later phases (log_*, ws_*, …) and we
// don't want a TS rebuild gating server-side enum additions.
export interface DebugEvent {
  at: string;
  session_id: string;
  kind: string;
  actor: string;
  text: string;
  meta?: Record<string, unknown>;
}

const MAX_BUFFER = 1000;

type Status = 'connecting' | 'open' | 'closed' | 'error';

interface UseDebugStreamOptions {
  /** Number of cold-start events to pull on mount. 0 disables. */
  initialLimit?: number;
  /** Server-side kind filter. Empty = accept all. */
  kinds?: string[];
}

/**
 * Subscribes to /api/debug/stream as an SSE source. Pre-fills with a
 * /api/debug/recent snapshot so the tab is never empty.
 *
 * Cap at MAX_BUFFER events; oldest drop first. EventSource handles
 * reconnect for free.
 */
export function useDebugStream({
  initialLimit = 200,
  kinds = [],
}: UseDebugStreamOptions = {}) {
  const [events, setEvents] = useState<DebugEvent[]>([]);
  const [paused, setPaused] = useState(false);
  const [status, setStatus] = useState<Status>('connecting');

  // Paused state needs to be readable from inside the SSE onmessage
  // callback without re-creating the EventSource on every toggle, so we
  // mirror it into a ref.
  const pausedRef = useRef(false);
  pausedRef.current = paused;

  const kindsKey = kinds.join(',');

  useEffect(() => {
    let cancelled = false;

    // 1. Cold-start snapshot via /recent so the tab paints immediately.
    const recentUrl = new URL(`${API_BASE || ''}/api/debug/recent`, window.location.origin);
    if (initialLimit > 0) recentUrl.searchParams.set('limit', String(initialLimit));
    if (kindsKey) recentUrl.searchParams.set('kinds', kindsKey);

    fetch(recentUrl.toString())
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then((body: { events?: DebugEvent[] }) => {
        if (cancelled || !body.events) return;
        setEvents(body.events.slice(-MAX_BUFFER));
      })
      .catch(() => {
        // Snapshot failure is non-fatal — the SSE stream below will
        // populate events as they fire. Surface via status.
      });

    // 2. Live stream.
    const streamUrl = new URL(`${API_BASE || ''}/api/debug/stream`, window.location.origin);
    if (kindsKey) streamUrl.searchParams.set('kinds', kindsKey);

    const es = new EventSource(streamUrl.toString());
    setStatus('connecting');

    es.onopen = () => {
      if (!cancelled) setStatus('open');
    };
    es.onerror = () => {
      // EventSource auto-reconnects; treat the error as transient. State
      // flips to 'error' until the next onopen.
      if (!cancelled) setStatus('error');
    };
    es.onmessage = (msg) => {
      if (cancelled || pausedRef.current) return;
      try {
        const parsed: DebugEvent = JSON.parse(msg.data);
        setEvents((prev) => {
          const next = prev.length >= MAX_BUFFER ? prev.slice(prev.length - MAX_BUFFER + 1) : prev;
          return [...next, parsed];
        });
      } catch {
        // ignore malformed frames
      }
    };

    return () => {
      cancelled = true;
      es.close();
      setStatus('closed');
    };
  }, [initialLimit, kindsKey]);

  const clear = useCallback(() => setEvents([]), []);

  return { events, paused, setPaused, status, clear };
}
