import { useEffect, useMemo, useRef, useState } from 'react';
import { useDebugStream, type DebugEvent } from '../hooks/useDebugStream';
import { TurnCard, groupTurns } from '../components/debug/TurnCard';

// Closed list of kinds the server can emit. Defined here so the filter
// chips are deterministic on cold load. Keep in sync with
// internal/transcript/event.go.
const KIND_OPTIONS: { kind: string; icon: string; color: string; severity?: 'error' | 'warn' }[] = [
  { kind: 'user_utterance',    icon: '🎙', color: '#58a6ff' },
  { kind: 'engineer_speech',   icon: '📢', color: '#bc8cff' },
  { kind: 'tool_call',         icon: '🔧', color: '#a5d6ff' },
  { kind: 'tool_result',       icon: '↩', color: '#a5d6ff' },
  { kind: 'analyst_query',     icon: '🧠', color: '#FF9300' },
  { kind: 'analyst_answer',    icon: '🧠', color: '#FF9300' },
  { kind: 'insight_pushed',    icon: '⚡', color: '#d29922' },
  { kind: 'event_dispatched',  icon: '➜', color: '#2ea043' },
  { kind: 'event_delivered',   icon: '✓', color: '#2ea043' },
  { kind: 'event_dropped',     icon: '✗', color: '#f85149', severity: 'error' },
  { kind: 'log_debug',         icon: '·', color: '#6e7681' },
  { kind: 'log_info',          icon: 'ⓘ', color: '#8b949e' },
  { kind: 'log_warn',          icon: '⚠', color: '#d29922', severity: 'warn' },
  { kind: 'log_error',         icon: '✗', color: '#f85149', severity: 'error' },
  { kind: 'ws_connected',      icon: '⇄', color: '#2ea043' },
  { kind: 'ws_disconnected',   icon: '⇆', color: '#8b949e' },
];

const KIND_META: Record<string, { icon: string; color: string; severity?: 'error' | 'warn' }> =
  Object.fromEntries(KIND_OPTIONS.map((k) => [k.kind, { icon: k.icon, color: k.color, severity: k.severity }]));

// log_debug is verbose; default it off on first load. Everything else on.
const DEFAULT_ACTIVE_KINDS = new Set(KIND_OPTIONS.map((k) => k.kind).filter((k) => k !== 'log_debug'));

function fmtTime(iso: string): string {
  const d = new Date(iso);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  const ms = String(d.getMilliseconds()).padStart(3, '0');
  return `${hh}:${mm}:${ss}.${ms}`;
}

function rowBg(severity?: 'error' | 'warn'): string | undefined {
  if (severity === 'error') return 'rgba(248, 81, 73, 0.08)';
  if (severity === 'warn') return 'rgba(210, 153, 34, 0.06)';
  return undefined;
}

function EventRow({ ev }: { ev: DebugEvent }) {
  const [expanded, setExpanded] = useState(false);
  const meta = KIND_META[ev.kind];
  const hasMeta = ev.meta && Object.keys(ev.meta).length > 0;

  return (
    <li
      className="border-l-2 px-3 py-1.5 hover:bg-panel/40 font-mono text-xs leading-snug"
      style={{ borderColor: meta?.color ?? '#30363d', background: rowBg(meta?.severity) }}
    >
      <div className="flex items-baseline gap-2">
        <span className="text-muted shrink-0 w-[80px]">{fmtTime(ev.at)}</span>
        <span className="shrink-0 w-4 text-center" title={ev.kind}>{meta?.icon ?? '·'}</span>
        <span
          className="shrink-0 text-[10px] uppercase tracking-wider px-1.5 rounded"
          style={{ color: meta?.color ?? '#8b949e', borderColor: meta?.color ?? '#30363d', borderWidth: 1 }}
        >
          {ev.kind}
        </span>
        <span className="shrink-0 text-[10px] text-muted">{ev.actor}</span>
        <span className="flex-1 text-text break-all">{ev.text || (hasMeta ? '(see meta)' : '')}</span>
        {hasMeta && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-[10px] text-muted hover:text-text"
          >
            {expanded ? 'hide' : 'meta'}
          </button>
        )}
      </div>
      {expanded && hasMeta && (
        <pre className="mt-1 ml-[100px] px-2 py-1.5 bg-bg border border-border rounded text-[11px] text-muted whitespace-pre-wrap break-all">
          {JSON.stringify(ev.meta, null, 2)}
        </pre>
      )}
    </li>
  );
}

export default function LiveDebug() {
  // We let the server send everything and filter client-side so toggling
  // checkboxes doesn't tear down the SSE connection.
  const { events, paused, setPaused, status, clear } = useDebugStream({ initialLimit: 200 });

  const [activeKinds, setActiveKinds] = useState<Set<string>>(new Set(DEFAULT_ACTIVE_KINDS));
  const [activeActors, setActiveActors] = useState<Set<string> | null>(null); // null = all
  const [grep, setGrep] = useState('');
  const [autoScroll, setAutoScroll] = useState(true);
  const [groupedView, setGroupedView] = useState(false);
  const listRef = useRef<HTMLUListElement>(null);

  // Dynamic actor universe — discovered from observed events. Sorted for
  // a stable filter UI even when actors come and go.
  const knownActors = useMemo(() => {
    const set = new Set<string>();
    for (const e of events) set.add(e.actor || 'unknown');
    return Array.from(set).sort();
  }, [events]);

  const visible = useMemo(() => {
    const needle = grep.trim().toLowerCase();
    return events.filter((e) => {
      if (!activeKinds.has(e.kind)) return false;
      if (activeActors && !activeActors.has(e.actor || 'unknown')) return false;
      if (needle) {
        const hay = `${e.text}\n${e.actor}\n${e.kind}\n${e.meta ? JSON.stringify(e.meta) : ''}`.toLowerCase();
        if (!hay.includes(needle)) return false;
      }
      return true;
    });
  }, [events, activeKinds, activeActors, grep]);

  useEffect(() => {
    if (!autoScroll || !listRef.current) return;
    listRef.current.scrollTop = listRef.current.scrollHeight;
  }, [visible, autoScroll]);

  const toggleKind = (kind: string) => {
    setActiveKinds((prev) => {
      const next = new Set(prev);
      if (next.has(kind)) next.delete(kind);
      else next.add(kind);
      return next;
    });
  };

  const toggleActor = (actor: string) => {
    setActiveActors((prev) => {
      const base = prev ?? new Set(knownActors);
      const next = new Set(base);
      if (next.has(actor)) next.delete(actor);
      else next.add(actor);
      // If user un-toggles to match the full set, drop the filter for
      // cheaper compares.
      if (next.size === knownActors.length) return null;
      return next;
    });
  };

  const exportJsonl = () => {
    const lines = visible.map((e) => JSON.stringify(e)).join('\n');
    const blob = new Blob([lines + '\n'], { type: 'application/jsonl' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `live-debug-${new Date().toISOString().replace(/[:.]/g, '-')}.jsonl`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  };

  const statusColor =
    status === 'open' ? '#2ea043' :
    status === 'connecting' ? '#d29922' :
    status === 'error' ? '#f85149' : '#888';

  // Anomaly summary (Phase 4): counts of errors / dropped events / tool
  // failures within the last 5 minutes of the buffer. Cheap O(n) scan.
  const anomalies = useMemo(() => {
    const cutoff = Date.now() - 5 * 60_000;
    let errors = 0;
    let dropped = 0;
    let toolErr = 0;
    for (const e of events) {
      const t = Date.parse(e.at);
      if (t < cutoff) continue;
      if (e.kind === 'log_error') errors++;
      else if (e.kind === 'event_dropped') dropped++;
      else if (e.kind === 'tool_result' && e.meta && typeof e.meta.error === 'string' && e.meta.error) toolErr++;
    }
    return { errors, dropped, toolErr };
  }, [events]);
  const anomalyTotal = anomalies.errors + anomalies.dropped + anomalies.toolErr;

  return (
    <div className="h-full flex flex-col p-4 gap-3">
      {/* Header */}
      <div className="flex items-center gap-3 flex-wrap">
        <h1 className="text-lg font-bold text-white">Live Debug</h1>
        <div className="flex items-center gap-1.5">
          <span className="w-2 h-2 rounded-full" style={{ background: statusColor }} />
          <span className="text-xs text-muted">{status}</span>
        </div>
        <span className="text-xs text-muted">{visible.length}/{events.length} events</span>

        {anomalyTotal > 0 && (
          <div className="flex items-center gap-2 text-xs">
            {anomalies.errors > 0 && (
              <span className="px-2 py-0.5 rounded border border-danger text-danger">
                {anomalies.errors} error{anomalies.errors === 1 ? '' : 's'} · 5m
              </span>
            )}
            {anomalies.dropped > 0 && (
              <span className="px-2 py-0.5 rounded border border-warning text-warning">
                {anomalies.dropped} dropped · 5m
              </span>
            )}
            {anomalies.toolErr > 0 && (
              <span className="px-2 py-0.5 rounded border border-warning text-warning">
                {anomalies.toolErr} tool err · 5m
              </span>
            )}
          </div>
        )}

        <div className="flex-1" />

        <input
          type="text"
          placeholder="grep…"
          value={grep}
          onChange={(e) => setGrep(e.target.value)}
          className="px-2.5 py-1 text-xs bg-bg border border-border rounded-md text-text font-mono w-48 focus:outline-none focus:border-accent"
        />
        <button
          type="button"
          onClick={() => setPaused((p) => !p)}
          className={`px-2.5 py-1 text-xs rounded-md border ${
            paused ? 'bg-warning/10 border-warning text-warning' : 'border-border text-text hover:border-text/40'
          }`}
        >
          {paused ? '▶ Resume' : '❚❚ Pause'}
        </button>
        <button
          type="button"
          onClick={() => setAutoScroll((s) => !s)}
          className={`px-2.5 py-1 text-xs rounded-md border ${
            autoScroll ? 'bg-accent/10 border-accent text-accent' : 'border-border text-text hover:border-text/40'
          }`}
          title="Stick to the latest event"
        >
          ↓ Auto-scroll
        </button>
        <button
          type="button"
          onClick={() => setGroupedView((g) => !g)}
          className={`px-2.5 py-1 text-xs rounded-md border ${
            groupedView ? 'bg-purple/10 border-purple text-purple' : 'border-border text-text hover:border-text/40'
          }`}
          title="Group user → tool → engineer events as turn cards"
          style={groupedView ? { color: '#bc8cff', borderColor: '#bc8cff', background: 'rgba(188,140,255,0.1)' } : undefined}
        >
          ⊞ Turns
        </button>
        <button
          type="button"
          onClick={exportJsonl}
          className="px-2.5 py-1 text-xs rounded-md border border-border text-text hover:border-text/40"
          title="Download visible rows as .jsonl"
        >
          ↓ JSONL
        </button>
        <button
          type="button"
          onClick={clear}
          className="px-2.5 py-1 text-xs rounded-md border border-border text-text hover:border-text/40"
        >
          Clear
        </button>
      </div>

      {/* Kind filter */}
      <div className="flex flex-wrap gap-1.5">
        {KIND_OPTIONS.map((opt) => {
          const active = activeKinds.has(opt.kind);
          return (
            <button
              type="button"
              key={opt.kind}
              onClick={() => toggleKind(opt.kind)}
              className={`px-2 py-0.5 text-[11px] font-mono rounded border transition-colors ${
                active ? 'text-white' : 'border-border text-muted hover:text-text'
              }`}
              style={active ? { borderColor: opt.color, background: `${opt.color}1a` } : undefined}
            >
              <span className="mr-1">{opt.icon}</span>
              {opt.kind}
            </button>
          );
        })}
      </div>

      {/* Actor filter (only shows when we have observed actors) */}
      {knownActors.length > 1 && (
        <div className="flex flex-wrap gap-1.5 items-center">
          <span className="text-[10px] text-muted uppercase tracking-wider">actor:</span>
          {knownActors.map((actor) => {
            const active = !activeActors || activeActors.has(actor);
            return (
              <button
                type="button"
                key={actor}
                onClick={() => toggleActor(actor)}
                className={`px-2 py-0.5 text-[11px] font-mono rounded border transition-colors ${
                  active ? 'border-accent/60 text-accent bg-accent/10' : 'border-border text-muted hover:text-text'
                }`}
              >
                {actor}
              </button>
            );
          })}
          {activeActors && (
            <button
              type="button"
              onClick={() => setActiveActors(null)}
              className="text-[10px] text-muted hover:text-text underline ml-1"
            >
              reset
            </button>
          )}
        </div>
      )}

      {/* Event list / grouped turns */}
      <ul
        ref={listRef}
        className={`flex-1 overflow-y-auto bg-bg/50 border border-border rounded-md ${
          groupedView ? 'p-2 space-y-2' : ''
        }`}
      >
        {visible.length === 0 ? (
          <li className="p-6 text-center text-muted text-sm">
            {events.length === 0 ? 'Waiting for events…' : 'No events match the current filter.'}
          </li>
        ) : groupedView ? (
          groupTurns(visible).map((bucket, i) =>
            bucket.length > 1 ? (
              <TurnCard key={`turn-${i}`} events={bucket} />
            ) : (
              <EventRow key={`${bucket[0].at}-${i}`} ev={bucket[0]} />
            ),
          )
        ) : (
          visible.map((ev, i) => (
            <EventRow key={`${ev.at}-${i}`} ev={ev} />
          ))
        )}
      </ul>
    </div>
  );
}
