import { useEffect, useRef, useState } from 'react';
import { API_BASE } from '../lib/constants';

// Activity mirrors mcpx.Activity on the Go side. Loose-typed because new
// kinds may land later (e.g. 'session_start') without a TS rebuild.
export interface PiActivity {
  id: number;
  at: string;
  kind:
    | 'tool_call'
    | 'tool_result'
    | 'trigger'
    | 'observation'
    | 'insight_pushed'
    | 'query_answer'
    | 'error'
    | string;
  tool?: string;
  persona?: string;
  /** Stable id for the specialist run this activity belongs to. Empty for system-level activities. */
  run_id?: string;
  args?: Record<string, unknown>;
  summary?: string;
  duration_ms?: number;
  meta?: Record<string, unknown>;
  error?: string;
}

/** PiRun is a derived view of all activities sharing a run_id. */
export interface PiRun {
  run_id: string;
  persona: string;
  started_at: string;
  last_at: string;
  duration_ms: number;
  activity_count: number;
  /** Most recent activity summary — useful as a one-line preview. */
  preview: string;
  /** Set when the run produced a final answer or observation. */
  finished: boolean;
  /** True if any activity in the run was an error. */
  has_error: boolean;
  /** True if any activity was a trigger of kind='query' (driver-asked). */
  trigger_kind?: string;
}

export interface PiPendingJob {
  job_id: string;
  question: string;
  context_topic: string;
  urgent?: boolean;
  started_at?: string;
}

export interface PiAgentSnapshot {
  enabled: boolean;
  mode?: string;
  provider?: string;
  trigger_depth?: number;
  subscribers?: number;
  pending_jobs?: PiPendingJob[];
  activities?: PiActivity[];
}

const MAX_BUFFER = 500;

type Status = 'connecting' | 'open' | 'closed' | 'error';

/**
 * Subscribes to /api/pi_agent/stream as an SSE source. Pre-fills with a
 * /api/pi_agent/recent snapshot so the page is never empty on cold start.
 */
export function usePiAgentStream({ initialLimit = 200 }: { initialLimit?: number } = {}) {
  const [activities, setActivities] = useState<PiActivity[]>([]);
  const [snapshot, setSnapshot] = useState<PiAgentSnapshot | null>(null);
  const [paused, setPaused] = useState(false);
  const [status, setStatus] = useState<Status>('connecting');

  const pausedRef = useRef(false);
  pausedRef.current = paused;

  useEffect(() => {
    let cancelled = false;

    // Cold-start snapshot.
    const recentUrl = new URL(`${API_BASE || ''}/api/pi_agent/recent`, window.location.origin);
    if (initialLimit > 0) recentUrl.searchParams.set('limit', String(initialLimit));

    fetch(recentUrl.toString())
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then((body: PiAgentSnapshot) => {
        if (cancelled) return;
        setSnapshot(body);
        setActivities((body.activities ?? []).slice(-MAX_BUFFER));
      })
      .catch(() => {
        // Non-fatal — SSE will keep us live.
      });

    // Live stream.
    const streamUrl = new URL(`${API_BASE || ''}/api/pi_agent/stream`, window.location.origin);
    const es = new EventSource(streamUrl.toString());
    setStatus('connecting');

    es.onopen = () => {
      if (!cancelled) setStatus('open');
    };
    es.onerror = () => {
      if (!cancelled) setStatus('error');
    };
    es.onmessage = (msg) => {
      if (cancelled || pausedRef.current) return;
      try {
        const parsed: PiActivity = JSON.parse(msg.data);
        setActivities((prev) => {
          const next = prev.length >= MAX_BUFFER ? prev.slice(prev.length - MAX_BUFFER + 1) : prev;
          return [...next, parsed];
        });
      } catch {
        /* ignore malformed frame */
      }
    };

    // Refresh the snapshot periodically so pending_jobs / status fields stay
    // accurate (the SSE only carries activities, not state metadata).
    const meta = setInterval(() => {
      fetch(recentUrl.toString())
        .then((r) => (r.ok ? r.json() : null))
        .then((body) => {
          if (body && !cancelled) setSnapshot(body);
        })
        .catch(() => {});
    }, 5000);

    return () => {
      cancelled = true;
      es.close();
      clearInterval(meta);
      setStatus('closed');
    };
  }, [initialLimit]);

  const clear = () => setActivities([]);

  return { activities, snapshot, status, paused, setPaused, clear };
}

/**
 * deriveRuns groups activities by run_id and computes per-run metadata
 * (persona, start, end, count, preview, status). Runs are returned newest
 * first. Activities without a run_id are excluded — they're system-level
 * (planner pull-loop pings, etc.) and don't represent a specialist
 * dispatch.
 */
export function deriveRuns(activities: PiActivity[]): PiRun[] {
  const byRun = new Map<string, PiActivity[]>();
  for (const a of activities) {
    const rid = a.run_id;
    if (!rid) continue;
    const bucket = byRun.get(rid);
    if (bucket) {
      bucket.push(a);
    } else {
      byRun.set(rid, [a]);
    }
  }
  const runs: PiRun[] = [];
  for (const [run_id, items] of byRun) {
    // items arrive in publish order (ascending id).
    const first = items[0];
    const last = items[items.length - 1];
    const startedMs = new Date(first.at).getTime();
    const lastMs = new Date(last.at).getTime();
    const persona =
      items.find((a) => a.persona)?.persona ?? '';
    const triggerAct = items.find((a) => a.kind === 'trigger');
    const triggerKind =
      (triggerAct?.meta?.kind as string | undefined) ?? undefined;
    const has_error = items.some((a) => a.kind === 'error');
    const finished = items.some(
      (a) => a.kind === 'query_answer' || a.kind === 'observation' || a.kind === 'insight_pushed',
    );
    runs.push({
      run_id,
      persona,
      started_at: first.at,
      last_at: last.at,
      duration_ms: Math.max(0, lastMs - startedMs),
      activity_count: items.length,
      preview: last.summary ?? last.tool ?? '',
      finished,
      has_error,
      trigger_kind: triggerKind,
    });
  }
  // Newest run first.
  runs.sort((a, b) => new Date(b.last_at).getTime() - new Date(a.last_at).getTime());
  return runs;
}

export async function askAnalystTeam(
  question: string,
  contextTopic: string = 'general',
  urgent: boolean = false,
): Promise<{ job_id: string; status: string; eta_seconds: number } | { error: string }> {
  const r = await fetch(`${API_BASE || ''}/api/analyst/query`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ question, context_topic: contextTopic, urgent }),
  });
  return r.json();
}
