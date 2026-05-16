import { useState } from 'react';
import type { DebugEvent } from '../../hooks/useDebugStream';

interface TurnCardProps {
  /** Events in chronological order that share the same turn_id. */
  events: DebugEvent[];
}

function pickTurnID(events: DebugEvent[]): string | undefined {
  for (const e of events) {
    const id = e.meta?.turn_id;
    if (typeof id === 'string' && id) return id;
  }
  return undefined;
}

function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  return `${hh}:${mm}:${ss}`;
}

export function TurnCard({ events }: TurnCardProps) {
  const [expanded, setExpanded] = useState(false);

  const turnID = pickTurnID(events);
  const user = events.find((e) => e.kind === 'user_utterance');
  const tools = events.filter((e) => e.kind === 'tool_call' || e.kind === 'tool_result');
  const engineers = events.filter((e) => e.kind === 'engineer_speech');
  const others = events.filter(
    (e) => !['user_utterance', 'tool_call', 'tool_result', 'engineer_speech'].includes(e.kind),
  );

  const start = events[0] ? Date.parse(events[0].at) : 0;
  const end = events[events.length - 1] ? Date.parse(events[events.length - 1].at) : 0;
  const totalMs = end - start;

  // Match tool_call to tool_result by call_id so the card can show
  // per-tool latency without re-running the math from meta.
  const toolPairs: { name: string; elapsedMs?: number; error?: string }[] = [];
  const inFlight: Record<string, { name: string }> = {};
  for (const ev of tools) {
    const callID = (ev.meta?.call_id as string | undefined) ?? '';
    const name = (ev.meta?.tool as string | undefined) ?? ev.text ?? '';
    if (ev.kind === 'tool_call') {
      inFlight[callID] = { name };
    } else {
      const open = inFlight[callID];
      delete inFlight[callID];
      toolPairs.push({
        name: open?.name || name,
        elapsedMs: typeof ev.meta?.elapsed_ms === 'number' ? (ev.meta.elapsed_ms as number) : undefined,
        error: typeof ev.meta?.error === 'string' ? (ev.meta.error as string) : undefined,
      });
    }
  }
  // Any unmatched tool_call (still in flight when the card renders).
  for (const callID of Object.keys(inFlight)) {
    toolPairs.push({ name: inFlight[callID].name });
  }

  const anyError = toolPairs.some((t) => t.error);

  return (
    <li
      className="rounded-md border bg-panel/40 hover:bg-panel/60 p-3"
      style={{ borderColor: anyError ? '#f85149' : '#30363d' }}
    >
      <div className="flex items-baseline gap-2 mb-2">
        <span className="text-[10px] text-muted font-mono">{events[0] ? fmtTime(events[0].at) : '—'}</span>
        {turnID && (
          <span className="text-[10px] text-muted font-mono opacity-60">turn {turnID.slice(0, 6)}</span>
        )}
        {totalMs > 0 && (
          <span className="text-[10px] text-accent font-mono">{fmtMs(totalMs)}</span>
        )}
        <span className="text-[10px] text-muted">
          {toolPairs.length > 0 && `${toolPairs.length} tool${toolPairs.length === 1 ? '' : 's'}`}
        </span>
        <div className="flex-1" />
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="text-[10px] text-muted hover:text-text underline"
        >
          {expanded ? 'collapse' : 'details'}
        </button>
      </div>

      {/* User line */}
      {user && (
        <div className="flex gap-2 items-start">
          <span className="text-[12px]">🎙</span>
          <p className="text-sm text-text flex-1 break-all">{user.text || <em className="text-muted">(empty)</em>}</p>
        </div>
      )}

      {/* Tool calls — collapsed by default */}
      {toolPairs.length > 0 && (
        <div className="ml-6 mt-2 space-y-1">
          {toolPairs.map((t, i) => (
            <div key={i} className="flex items-center gap-2 text-xs font-mono">
              <span style={{ color: t.error ? '#f85149' : '#a5d6ff' }}>🔧 {t.name}</span>
              {t.elapsedMs !== undefined && (
                <span className="text-muted">{fmtMs(t.elapsedMs)}</span>
              )}
              {t.error && <span className="text-danger">err: {t.error.slice(0, 80)}</span>}
              {t.elapsedMs === undefined && !t.error && (
                <span className="text-warning">in flight…</span>
              )}
            </div>
          ))}
        </div>
      )}

      {/* Engineer reply */}
      {engineers.map((e, i) => (
        <div key={i} className="flex gap-2 items-start mt-2">
          <span className="text-[12px]">📢</span>
          <p className="text-sm text-text flex-1 break-all" style={{ color: '#bc8cff' }}>{e.text}</p>
        </div>
      ))}

      {/* Expanded view: raw JSON for every event in the turn */}
      {expanded && (
        <pre className="mt-3 px-2 py-1.5 bg-bg border border-border rounded text-[11px] text-muted whitespace-pre-wrap break-all font-mono max-h-[400px] overflow-y-auto">
          {events.map((e) => JSON.stringify(e)).join('\n')}
        </pre>
      )}

      {/* Any other kinds that landed under this turn id (rare) */}
      {others.length > 0 && (
        <div className="mt-2 text-[10px] text-muted font-mono">
          + {others.length} other event{others.length === 1 ? '' : 's'}: {others.map((o) => o.kind).join(', ')}
        </div>
      )}
    </li>
  );
}

/**
 * Groups a flat event stream into turn buckets. An event with a turn_id
 * meta field anchors a bucket; events without are passed through as
 * single-event buckets. Returns events in chronological order.
 */
export function groupTurns(events: DebugEvent[]): DebugEvent[][] {
  const buckets: Record<string, DebugEvent[]> = {};
  const out: DebugEvent[][] = [];
  for (const ev of events) {
    const tid = ev.meta?.turn_id;
    if (typeof tid === 'string' && tid) {
      if (!buckets[tid]) {
        buckets[tid] = [];
        out.push(buckets[tid]);
      }
      buckets[tid].push(ev);
    } else {
      out.push([ev]);
    }
  }
  return out;
}
