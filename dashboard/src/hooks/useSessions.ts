import { useEffect, useState } from 'react';
import { API_BASE } from '../lib/constants';
import type {
  SessionListItem,
  SessionSummary,
  TracesResponse,
  CompareResponse,
  LapDeltaResponse,
  LapListResponse,
} from '../types/sessions';

export function useSessionsList() {
  const [sessions, setSessions] = useState<SessionListItem[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    fetch(`${API_BASE}/api/sessions`)
      .then(async (r) => {
        if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
        return (await r.json()) as SessionListItem[];
      })
      .then((data) => {
        if (!cancelled) {
          setSessions(data);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return { sessions, loading, error };
}

export function useSessionSummary(uid: string | null) {
  const [summary, setSummary] = useState<SessionSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!uid) {
      setSummary(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    fetch(`${API_BASE}/api/sessions/${encodeURIComponent(uid)}/summary`)
      .then(async (r) => {
        if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
        return (await r.json()) as SessionSummary;
      })
      .then((data) => {
        if (!cancelled) {
          setSummary(data);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [uid]);

  return { summary, loading, error };
}

interface TracesQuery {
  laps: string;
  channels: string;
  buckets: number;
}

export function useLapTraces(uid: string | null, query: TracesQuery) {
  const [data, setData] = useState<TracesResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!uid) {
      setData(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    const params = new URLSearchParams({
      session_uid: uid,
      laps: query.laps,
      channels: query.channels,
      buckets: query.buckets.toString(),
    });
    fetch(`${API_BASE}/api/laps/traces?${params.toString()}`)
      .then(async (r) => {
        if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
        return (await r.json()) as TracesResponse;
      })
      .then((d) => {
        if (!cancelled) {
          setData(d);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [uid, query.laps, query.channels, query.buckets]);

  return { data, loading, error };
}

export function useLapCompare(uid: string | null, lap: number | null) {
  const [data, setData] = useState<CompareResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!uid) {
      setData(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    const params = new URLSearchParams({ session_uid: uid });
    if (lap != null) params.set('lap', lap.toString());
    fetch(`${API_BASE}/api/laps/compare?${params.toString()}`)
      .then(async (r) => {
        if (!r.ok) {
          if (r.status === 404) return null; // no best baseline yet
          throw new Error(`${r.status} ${r.statusText}`);
        }
        return (await r.json()) as CompareResponse;
      })
      .then((d) => {
        if (!cancelled) {
          setData(d);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [uid, lap]);

  return { data, loading, error };
}

interface LapDeltaQuery {
  lap: string | null; // null = handler default ("last"); pass 'last' or 'N'
  reference: string | null; // null = "best"; pass 'best' or 'N'
  buckets: number;
  // Optional cross-session overrides — when set, the lap (and/or reference)
  // is looked up in the named session. Backend rejects requests whose
  // track_length doesn't match the resolving session.
  lapSessionUid?: string | null;
  referenceSessionUid?: string | null;
}

// useLapDelta fetches /api/laps/delta. Re-fires whenever any query param
// changes — typically driven by lap-picker selections in the compare UI.
// uid="" or null disables the fetch entirely (return placeholder state).
export function useLapDelta(uid: string | null, query: LapDeltaQuery) {
  const [data, setData] = useState<LapDeltaResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!uid) {
      setData(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    const params = new URLSearchParams({
      session_uid: uid,
      buckets: query.buckets.toString(),
    });
    if (query.lap) params.set('lap', query.lap);
    if (query.reference) params.set('reference', query.reference);
    if (query.lapSessionUid) params.set('lap_session_uid', query.lapSessionUid);
    if (query.referenceSessionUid)
      params.set('reference_session_uid', query.referenceSessionUid);
    fetch(`${API_BASE}/api/laps/delta?${params.toString()}`)
      .then(async (r) => {
        if (!r.ok) {
          if (r.status === 404) return null;
          throw new Error(`${r.status} ${r.statusText}`);
        }
        return (await r.json()) as LapDeltaResponse;
      })
      .then((d) => {
        if (!cancelled) {
          setData(d);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [
    uid,
    query.lap,
    query.reference,
    query.buckets,
    query.lapSessionUid,
    query.referenceSessionUid,
  ]);

  return { data, loading, error };
}

// useLapList fetches /api/laps/list — lightweight roster for the lap picker.
// Polls every `refreshMs` so a live session's roster grows as laps complete.
// Pass refreshMs=0 to disable polling (e.g. for completed sessions).
export function useLapList(uid: string | null, refreshMs: number = 5000) {
  const [data, setData] = useState<LapListResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!uid) {
      setData(null);
      return;
    }
    let cancelled = false;
    const tick = () => {
      const params = new URLSearchParams({ session_uid: uid });
      setLoading(true);
      fetch(`${API_BASE}/api/laps/list?${params.toString()}`)
        .then(async (r) => {
          if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
          return (await r.json()) as LapListResponse;
        })
        .then((d) => {
          if (!cancelled) {
            setData(d);
            setError(null);
          }
        })
        .catch((e) => {
          if (!cancelled) setError(e instanceof Error ? e.message : String(e));
        })
        .finally(() => {
          if (!cancelled) setLoading(false);
        });
    };
    tick();
    if (refreshMs <= 0) {
      return () => {
        cancelled = true;
      };
    }
    const timer = setInterval(tick, refreshMs);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [uid, refreshMs]);

  return { data, loading, error };
}
