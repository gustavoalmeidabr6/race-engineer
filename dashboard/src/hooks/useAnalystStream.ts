import { useEffect, useRef, useState } from 'react';
import { API_BASE } from '../lib/constants';

// AnalystActivity mirrors analyst.Activity on the Go side. Loose-typed
// because new ACP `session/update` kinds may surface as additional values
// for `kind` without a TS rebuild.
export interface AnalystActivity {
  id: number;
  at: string;
  kind:
    | 'runtime'
    | 'trigger'
    | 'session_start'
    | 'message_chunk'
    | 'tool_call'
    | 'tool_result'
    | 'file_write'
    | 'file_read'
    | 'permission'
    | 'insight_pushed'
    | 'answer'
    | 'error'
    | string;
  session_id?: string;
  /** Stable id for the prompt dispatch this activity belongs to. Empty for runtime/session events. */
  run_id?: string;
  /** "lap_complete" | "significant_event" | "query" — populated only on the trigger kind. */
  trigger?: string;
  tool?: string;
  summary?: string;
  duration_ms?: number;
  meta?: Record<string, unknown>;
  error?: string;
}

/** AnalystRun is a derived view of all activities sharing a run_id. */
export interface AnalystRun {
  run_id: string;
  trigger: string;
  started_at: string;
  last_at: string;
  duration_ms: number;
  activity_count: number;
  /** Most recent activity summary — a one-line preview for the run row. */
  preview: string;
  /** Set when the run produced a final answer / insight / file_write. */
  finished: boolean;
  /** True if any activity in the run was an error. */
  has_error: boolean;
}

export interface AnalystPendingJob {
  job_id: string;
  question: string;
  context_topic: string;
  urgent?: boolean;
  started_at?: string;
  run_id?: string;
}

export interface AnalystSnapshot {
  enabled: boolean;
  ready?: boolean;
  subscribers?: number;
  pending_jobs?: AnalystPendingJob[];
  activities?: AnalystActivity[];
}

export interface AnalystStatus {
  installed: boolean;
  ready: boolean;
  binary?: string;
  version?: string;
  workspace?: string;
  mcp_url?: string;
  child_pid?: number;
  started_at?: string;
  session_id?: string;
  subscribers?: number;
  reason?: string;
}

const MAX_BUFFER = 500;

type Status = 'connecting' | 'open' | 'closed' | 'error';

/**
 * Subscribes to /api/analyst/stream as an SSE source. Pre-fills with a
 * /api/analyst/recent snapshot so the page is never empty on cold start.
 */
export function useAnalystStream({ initialLimit = 200 }: { initialLimit?: number } = {}) {
  const [activities, setActivities] = useState<AnalystActivity[]>([]);
  const [snapshot, setSnapshot] = useState<AnalystSnapshot | null>(null);
  const [paused, setPaused] = useState(false);
  const [status, setStatus] = useState<Status>('connecting');

  const pausedRef = useRef(false);
  pausedRef.current = paused;

  useEffect(() => {
    let cancelled = false;

    const recentUrl = new URL(`${API_BASE || ''}/api/analyst/recent`, window.location.origin);
    if (initialLimit > 0) recentUrl.searchParams.set('limit', String(initialLimit));

    fetch(recentUrl.toString())
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then((body: AnalystSnapshot) => {
        if (cancelled) return;
        setSnapshot(body);
        setActivities((body.activities ?? []).slice(-MAX_BUFFER));
      })
      .catch(() => {
        // Non-fatal — SSE will keep us live.
      });

    const streamUrl = new URL(`${API_BASE || ''}/api/analyst/stream`, window.location.origin);
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
        const parsed: AnalystActivity = JSON.parse(msg.data);
        setActivities((prev) => {
          const next =
            prev.length >= MAX_BUFFER ? prev.slice(prev.length - MAX_BUFFER + 1) : prev;
          return [...next, parsed];
        });
      } catch {
        /* ignore malformed frame */
      }
    };

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
 * deriveRuns groups activities by run_id, computing per-run metadata
 * (trigger kind, start, end, count, preview, status). Runs are returned
 * newest first. Activities without a run_id (runtime / session_start) are
 * filtered out — they represent infrastructure, not a prompt dispatch.
 */
export function deriveRuns(activities: AnalystActivity[]): AnalystRun[] {
  const byRun = new Map<string, AnalystActivity[]>();
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
  const runs: AnalystRun[] = [];
  for (const [run_id, items] of byRun) {
    const first = items[0];
    const last = items[items.length - 1];
    const startedMs = new Date(first.at).getTime();
    const lastMs = new Date(last.at).getTime();
    const triggerAct = items.find((a) => a.kind === 'trigger');
    const trigger = triggerAct?.trigger ?? '';
    const has_error = items.some((a) => a.kind === 'error');
    const finished = items.some(
      (a) => a.kind === 'answer' || a.kind === 'insight_pushed' || a.kind === 'file_write',
    );
    runs.push({
      run_id,
      trigger,
      started_at: first.at,
      last_at: last.at,
      duration_ms: Math.max(0, lastMs - startedMs),
      activity_count: items.length,
      preview: last.summary ?? last.tool ?? '',
      finished,
      has_error,
    });
  }
  runs.sort((a, b) => new Date(b.last_at).getTime() - new Date(a.last_at).getTime());
  return runs;
}

export async function askAnalyst(
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

export async function fetchAnalystStatus(): Promise<AnalystStatus> {
  const r = await fetch(`${API_BASE || ''}/api/analyst/status`);
  if (!r.ok) {
    return { installed: false, ready: false, reason: `HTTP ${r.status}` };
  }
  return r.json();
}

// ── Message-bubble grouping ──────────────────────────────────────────────
//
// The raw activity stream has one row per token chunk and per tool call,
// which is unreadable as a list. groupRunIntoSegments collapses the chunks
// for one run into ChatGPT-style message segments: prose runs (with
// optional "thought" italic), expandable tool widgets, file ops, etc.

export type AnalystSegment =
  | { kind: 'text'; content: string; thought: boolean; startedAt: string; lastAt: string; activityIds: number[] }
  | {
      kind: 'tool';
      tool: string;
      status: 'pending' | 'done' | 'error';
      rawInput?: unknown;
      summary?: string;
      durationMs?: number;
      startedAt: string;
      lastAt: string;
      activityIds: number[];
    }
  | { kind: 'file'; op: 'read' | 'write'; path: string; at: string; activityId: number }
  | { kind: 'permission'; summary: string; at: string; activityId: number }
  | { kind: 'answer'; summary: string; jobId?: string; at: string; activityId: number }
  | { kind: 'insight'; summary: string; priority?: number; at: string; activityId: number }
  | { kind: 'error'; summary: string; at: string; activityId: number };

/**
 * Collapse one run's activities into render-friendly segments. The order of
 * segments matches the original chronological order. Consecutive message
 * chunks of the same kind (regular vs thought) merge into a single text
 * segment; tool_call + tool_result with matching tool_call_id merge into a
 * single tool segment.
 */
export function groupRunIntoSegments(items: AnalystActivity[]): AnalystSegment[] {
  const segments: AnalystSegment[] = [];
  // toolIndex maps tool_call_id → segments index, so a later tool_result
  // can find the segment to mutate. Falls back to "latest pending tool" if
  // tool_call_id is missing.
  const toolIndex = new Map<string, number>();

  for (const a of items) {
    if (a.kind === 'trigger' || a.kind === 'session_start' || a.kind === 'runtime') {
      // These are run-level markers rendered by the bubble header, not as
      // body segments.
      continue;
    }
    if (a.kind === 'message_chunk') {
      const thought = a.meta?.chunk_kind === 'agent_thought_chunk';
      const text = a.summary ?? '';
      if (!text) continue;
      const last = segments[segments.length - 1];
      if (last && last.kind === 'text' && last.thought === thought) {
        last.content += text;
        last.lastAt = a.at;
        last.activityIds.push(a.id);
      } else {
        segments.push({
          kind: 'text',
          content: text,
          thought,
          startedAt: a.at,
          lastAt: a.at,
          activityIds: [a.id],
        });
      }
      continue;
    }
    if (a.kind === 'tool_call' || a.kind === 'tool_result') {
      const callID = String(a.meta?.tool_call_id ?? '');
      const isResult = a.kind === 'tool_result';
      const existingIdx = callID ? toolIndex.get(callID) : undefined;
      const status: 'pending' | 'done' | 'error' =
        a.error
          ? 'error'
          : isResult
          ? 'done'
          : (a.meta?.status === 'completed' ? 'done' : 'pending');
      if (existingIdx !== undefined) {
        const seg = segments[existingIdx];
        if (seg.kind === 'tool') {
          seg.lastAt = a.at;
          seg.status = status === 'pending' ? seg.status : status;
          seg.activityIds.push(a.id);
          if (a.duration_ms) seg.durationMs = a.duration_ms;
          if (a.summary) seg.summary = a.summary;
          if (a.meta?.raw_input !== undefined && seg.rawInput === undefined) {
            seg.rawInput = a.meta.raw_input;
          }
        }
        continue;
      }
      const idx = segments.length;
      segments.push({
        kind: 'tool',
        tool: a.tool ?? '(tool)',
        status,
        rawInput: a.meta?.raw_input,
        summary: a.summary,
        durationMs: a.duration_ms,
        startedAt: a.at,
        lastAt: a.at,
        activityIds: [a.id],
      });
      if (callID) toolIndex.set(callID, idx);
      continue;
    }
    if (a.kind === 'file_read' || a.kind === 'file_write') {
      segments.push({
        kind: 'file',
        op: a.kind === 'file_read' ? 'read' : 'write',
        path: a.summary ?? a.tool ?? '(file)',
        at: a.at,
        activityId: a.id,
      });
      continue;
    }
    if (a.kind === 'permission') {
      segments.push({
        kind: 'permission',
        summary: a.summary ?? 'permission request',
        at: a.at,
        activityId: a.id,
      });
      continue;
    }
    if (a.kind === 'answer') {
      segments.push({
        kind: 'answer',
        summary: a.summary ?? '',
        jobId: a.meta?.job_id ? String(a.meta.job_id) : undefined,
        at: a.at,
        activityId: a.id,
      });
      continue;
    }
    if (a.kind === 'insight_pushed') {
      segments.push({
        kind: 'insight',
        summary: a.summary ?? '',
        priority:
          typeof a.meta?.priority === 'number' ? (a.meta.priority as number) : undefined,
        at: a.at,
        activityId: a.id,
      });
      continue;
    }
    if (a.kind === 'error') {
      segments.push({
        kind: 'error',
        summary: a.summary ?? a.error ?? 'error',
        at: a.at,
        activityId: a.id,
      });
      continue;
    }
    // Unknown kind — surface as a text segment so nothing is silently dropped.
    if (a.summary) {
      segments.push({
        kind: 'text',
        content: `[${a.kind}] ${a.summary}`,
        thought: false,
        startedAt: a.at,
        lastAt: a.at,
        activityIds: [a.id],
      });
    }
  }
  return segments;
}

/**
 * groupByRun returns a [run_id, activities[]] tuple list ordered oldest →
 * newest by the run's first activity timestamp. Activities without a
 * run_id are bucketed under "" so the caller can render system / runtime
 * events separately.
 */
export function groupByRun(
  activities: AnalystActivity[],
): { run_id: string; items: AnalystActivity[] }[] {
  const order: string[] = [];
  const byRun = new Map<string, AnalystActivity[]>();
  for (const a of activities) {
    const rid = a.run_id ?? '';
    const bucket = byRun.get(rid);
    if (bucket) {
      bucket.push(a);
    } else {
      byRun.set(rid, [a]);
      order.push(rid);
    }
  }
  return order.map((rid) => ({ run_id: rid, items: byRun.get(rid)! }));
}
