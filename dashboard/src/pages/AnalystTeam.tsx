import { useMemo, useRef, useState, useEffect } from 'react';
import {
  useAnalystStream,
  askAnalyst,
  deriveRuns,
  fetchAnalystStatus,
  groupByRun,
  groupRunIntoSegments,
  type AnalystActivity,
  type AnalystRun,
  type AnalystSegment,
  type AnalystStatus,
} from '../hooks/useAnalystStream';

// Bubble UI: each run is one chat-style message card. Inside the card we
// interleave streamed text, tool widgets, and final answers in
// chronological order. Replaces the per-token row feed.

function fmtTime(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleTimeString('en-GB', { hour12: false }) + '.' +
      String(d.getMilliseconds()).padStart(3, '0');
  } catch {
    return iso;
  }
}

function fmtDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rs = Math.floor(s % 60);
  return `${m}m ${rs}s`;
}

const TRIGGER_BADGES: Record<string, { bg: string; fg: string; label: string }> = {
  query:              { bg: '#1e2a4a', fg: '#7aa2ff', label: 'QUERY' },
  lap_complete:       { bg: '#1a221a', fg: '#9bd49b', label: 'LAP' },
  significant_event:  { bg: '#2a261f', fg: '#f0c674', label: 'EVENT' },
};

function triggerBadge(trigger: string) {
  return TRIGGER_BADGES[trigger] ?? { bg: '#1f1f1f', fg: '#bbbbbb', label: trigger.toUpperCase() || 'RUN' };
}

// ── Segment renderers ───────────────────────────────────────────────────

function TextSegment({ seg, streaming }: { seg: Extract<AnalystSegment, { kind: 'text' }>; streaming: boolean }) {
  const base = seg.thought
    ? 'italic text-muted'
    : 'text-text';
  return (
    <div className={`text-[13px] leading-relaxed whitespace-pre-wrap break-words ${base}`}>
      {seg.thought && (
        <span className="text-[10px] uppercase tracking-wider mr-2 text-muted">thinking</span>
      )}
      {seg.content}
      {streaming && <span className="inline-block w-2 h-3 align-middle bg-accent ml-0.5 animate-pulse" />}
    </div>
  );
}

function ToolSegment({ seg }: { seg: Extract<AnalystSegment, { kind: 'tool' }> }) {
  const [open, setOpen] = useState(false);
  const statusDot =
    seg.status === 'error'
      ? '#f85149'
      : seg.status === 'pending'
      ? '#d29922'
      : '#2ea043';
  const inputStr = useMemo(() => {
    if (seg.rawInput === undefined || seg.rawInput === null) return '';
    if (typeof seg.rawInput === 'string') return seg.rawInput;
    try {
      return JSON.stringify(seg.rawInput, null, 2);
    } catch {
      return String(seg.rawInput);
    }
  }, [seg.rawInput]);
  const hasDetail = !!inputStr;
  return (
    <div className="rounded border border-border/60 bg-bg/60 overflow-hidden">
      <button
        type="button"
        onClick={() => hasDetail && setOpen((o) => !o)}
        className={`w-full flex items-center gap-2 px-2.5 py-1.5 text-left ${hasDetail ? 'hover:bg-bg' : 'cursor-default'}`}
      >
        <span
          className={`w-1.5 h-1.5 rounded-full shrink-0 ${seg.status === 'pending' ? 'animate-pulse' : ''}`}
          style={{ backgroundColor: statusDot }}
        />
        <span className="text-[10px] uppercase tracking-wider text-muted shrink-0">tool</span>
        <span className="text-[12px] font-mono text-accent truncate">{seg.tool}</span>
        {seg.status === 'pending' && (
          <span className="text-[10px] text-amber-400 ml-1">running…</span>
        )}
        {seg.durationMs !== undefined && seg.status !== 'pending' && (
          <span className="text-[10px] text-muted ml-auto">{fmtDuration(seg.durationMs)}</span>
        )}
        {hasDetail && (
          <span className="text-[10px] text-muted ml-1">{open ? '▾' : '▸'}</span>
        )}
      </button>
      {open && hasDetail && (
        <pre className="text-[11px] font-mono text-muted whitespace-pre-wrap break-words px-2.5 py-1.5 border-t border-border/40 bg-panel/40 max-h-64 overflow-auto">
          {inputStr}
        </pre>
      )}
    </div>
  );
}

function FileSegment({ seg }: { seg: Extract<AnalystSegment, { kind: 'file' }> }) {
  const isWrite = seg.op === 'write';
  return (
    <div className="flex items-center gap-2 text-[12px] px-2.5 py-1 rounded bg-bg/60 border border-border/60">
      <span className={`text-[10px] uppercase tracking-wider ${isWrite ? 'text-emerald-400' : 'text-blue-400'}`}>
        {seg.op === 'write' ? 'wrote' : 'read'}
      </span>
      <span className="font-mono text-text truncate">{seg.path}</span>
    </div>
  );
}

function PermissionSegment({ seg }: { seg: Extract<AnalystSegment, { kind: 'permission' }> }) {
  return (
    <div className="text-[12px] px-2.5 py-1.5 rounded bg-amber-950/30 border border-amber-700/40 text-amber-300">
      <span className="text-[10px] uppercase tracking-wider mr-2">permission</span>
      {seg.summary}
    </div>
  );
}

function AnswerSegment({ seg }: { seg: Extract<AnalystSegment, { kind: 'answer' }> }) {
  if (!seg.summary) return null;
  return (
    <div className="px-3 py-2 rounded bg-amber-950/30 border border-amber-700/40 text-[13px] text-amber-100">
      <div className="text-[10px] uppercase tracking-wider text-amber-400 mb-1">final answer{seg.jobId ? ` · ${seg.jobId}` : ''}</div>
      <div className="whitespace-pre-wrap break-words">{seg.summary}</div>
    </div>
  );
}

function InsightSegment({ seg }: { seg: Extract<AnalystSegment, { kind: 'insight' }> }) {
  return (
    <div className="px-3 py-2 rounded bg-orange-950/30 border border-orange-700/40 text-[13px] text-orange-200">
      <div className="text-[10px] uppercase tracking-wider text-orange-400 mb-1">
        radio → driver{seg.priority !== undefined ? ` · P${seg.priority}` : ''}
      </div>
      <div className="whitespace-pre-wrap break-words">{seg.summary}</div>
    </div>
  );
}

function ErrorSegment({ seg }: { seg: Extract<AnalystSegment, { kind: 'error' }> }) {
  return (
    <div className="px-2.5 py-1.5 rounded bg-red-950/30 border border-red-700/40 text-[12px] text-red-300">
      <span className="text-[10px] uppercase tracking-wider mr-2">error</span>
      {seg.summary}
    </div>
  );
}

// ── Run bubble ──────────────────────────────────────────────────────────

function RunBubble({
  run,
  items,
  expandedDefault,
}: {
  run: AnalystRun;
  items: AnalystActivity[];
  expandedDefault: boolean;
}) {
  const segments = useMemo(() => groupRunIntoSegments(items), [items]);
  const [open, setOpen] = useState(expandedDefault);
  const triggerAct = useMemo(() => items.find((a) => a.kind === 'trigger'), [items]);
  const question = triggerAct?.summary ?? run.preview ?? '';
  const jobID = triggerAct?.meta?.job_id ? String(triggerAct.meta.job_id) : undefined;
  const ageMs = Date.now() - new Date(run.last_at).getTime();
  const live = !run.finished && ageMs < 30_000;
  const t = triggerBadge(run.trigger || '');

  // Streaming indicator only applies to the last text segment.
  const lastTextIdx = useMemo(() => {
    for (let i = segments.length - 1; i >= 0; i -= 1) {
      if (segments[i].kind === 'text') return i;
    }
    return -1;
  }, [segments]);

  return (
    <div className="rounded-lg border border-border/70 bg-panel/40 overflow-hidden">
      {/* Bubble header */}
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="w-full flex items-start gap-3 px-3 py-2 text-left hover:bg-bg/40 transition-colors"
      >
        <span
          className="text-[10px] font-bold uppercase tracking-wider px-2 py-0.5 rounded mt-0.5 shrink-0"
          style={{ backgroundColor: t.bg, color: t.fg }}
        >
          {t.label}
        </span>
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 text-[11px] text-muted">
            <span className="font-mono text-accent">{run.run_id}</span>
            {jobID && <span className="font-mono">job {jobID}</span>}
            <span className="ml-auto">{fmtTime(run.started_at)}</span>
          </div>
          <div className="text-[13px] text-text mt-0.5 line-clamp-2 break-words">
            {question || '(no prompt)'}
          </div>
        </div>
        <div className="flex flex-col items-end gap-1 shrink-0 mt-0.5">
          <div className="flex items-center gap-1.5 text-[10px]">
            {live ? (
              <>
                <span className="w-1.5 h-1.5 rounded-full bg-amber-400 animate-pulse" />
                <span className="text-amber-400">running</span>
              </>
            ) : run.has_error ? (
              <>
                <span className="w-1.5 h-1.5 rounded-full bg-red-500" />
                <span className="text-red-400">error</span>
              </>
            ) : run.finished ? (
              <>
                <span className="w-1.5 h-1.5 rounded-full bg-emerald-500" />
                <span className="text-emerald-400">done</span>
              </>
            ) : (
              <>
                <span className="w-1.5 h-1.5 rounded-full bg-muted" />
                <span className="text-muted">idle</span>
              </>
            )}
          </div>
          <div className="text-[10px] text-muted">
            {fmtDuration(run.duration_ms || 0)} · {run.activity_count} steps
          </div>
        </div>
        <span className="text-muted text-[11px] ml-1 mt-0.5">{open ? '▾' : '▸'}</span>
      </button>

      {/* Bubble body */}
      {open && (
        <div className="border-t border-border/50 px-3 py-2 space-y-2">
          {segments.length === 0 && (
            <div className="text-[12px] text-muted italic">No content yet — waiting for opencode…</div>
          )}
          {segments.map((seg, i) => {
            const key = `${i}-${seg.kind}`;
            switch (seg.kind) {
              case 'text':
                return <TextSegment key={key} seg={seg} streaming={live && i === lastTextIdx} />;
              case 'tool':
                return <ToolSegment key={key} seg={seg} />;
              case 'file':
                return <FileSegment key={key} seg={seg} />;
              case 'permission':
                return <PermissionSegment key={key} seg={seg} />;
              case 'answer':
                return <AnswerSegment key={key} seg={seg} />;
              case 'insight':
                return <InsightSegment key={key} seg={seg} />;
              case 'error':
                return <ErrorSegment key={key} seg={seg} />;
            }
          })}
        </div>
      )}
    </div>
  );
}

// SystemEventStrip renders the small handful of activities that have no
// run_id (runtime lifecycle, session_start). Keeps them out of the main
// chat flow but still visible.
function SystemEventStrip({ items }: { items: AnalystActivity[] }) {
  if (items.length === 0) return null;
  return (
    <div className="rounded border border-border/40 bg-bg/40 px-3 py-2 space-y-1">
      <div className="text-[10px] uppercase tracking-wider text-muted">System</div>
      {items.slice(-6).map((a) => (
        <div key={a.id} className="text-[11px] font-mono flex gap-2">
          <span className="text-muted shrink-0">{fmtTime(a.at)}</span>
          <span className="text-muted shrink-0 w-[80px]">{a.kind}</span>
          <span className="text-text/80 break-words flex-1 min-w-0">
            {a.summary || a.error || ''}
          </span>
        </div>
      ))}
    </div>
  );
}

// ── Sidebar run card ────────────────────────────────────────────────────

function RunCard({
  run,
  selected,
  onClick,
}: {
  run: AnalystRun;
  selected: boolean;
  onClick: () => void;
}) {
  const isRunning = !run.finished && Date.now() - new Date(run.last_at).getTime() < 90_000;
  const dot = run.has_error
    ? '#f85149'
    : isRunning
    ? '#d29922'
    : run.finished
    ? '#2ea043'
    : '#6e7681';
  const t = triggerBadge(run.trigger || '');
  return (
    <button
      type="button"
      onClick={onClick}
      className={`w-full text-left px-3 py-2 border-b border-border/40 transition-colors ${
        selected ? 'bg-accent/15 border-l-2 border-l-accent' : 'hover:bg-bg/50'
      }`}
    >
      <div className="flex items-center gap-2">
        <span
          className={`w-1.5 h-1.5 rounded-full shrink-0 ${isRunning ? 'animate-pulse' : ''}`}
          style={{ backgroundColor: dot }}
        />
        <span
          className="text-[9px] font-bold uppercase tracking-wider px-1.5 py-0.5 rounded shrink-0"
          style={{ backgroundColor: t.bg, color: t.fg }}
        >
          {t.label}
        </span>
        <span className="text-[11px] font-mono text-accent truncate">{run.run_id}</span>
        <span className="ml-auto text-[10px] text-muted whitespace-nowrap">{run.activity_count}</span>
      </div>
      <div className="mt-1 text-[11px] text-text line-clamp-2 break-words">
        {run.preview || '(no preview)'}
      </div>
      <div className="mt-1 flex items-center gap-2 text-[10px] text-muted">
        <span>{fmtTime(run.started_at)}</span>
        <span>·</span>
        <span>{fmtDuration(run.duration_ms || 0)}</span>
      </div>
    </button>
  );
}

function StatusBadge({
  ready,
  version,
  pendingCount,
  status,
}: {
  ready: boolean;
  version?: string;
  pendingCount: number;
  status: string;
}) {
  const dot =
    status === 'open' ? '#2ea043' : status === 'error' ? '#f85149' : '#d29922';
  return (
    <div className="flex items-center gap-3 text-[12px]">
      <div className="flex items-center gap-1.5">
        <span className="w-2 h-2 rounded-full" style={{ backgroundColor: dot }} />
        <span className="text-muted">stream</span>
        <span className="text-text font-semibold">{status}</span>
      </div>
      <div className="flex items-center gap-1.5">
        <span className="text-muted">runtime</span>
        <span className={`font-semibold ${ready ? 'text-accent' : 'text-muted'}`}>
          {ready ? 'ready' : 'starting'}
        </span>
      </div>
      {version && (
        <div className="flex items-center gap-1.5">
          <span className="text-muted">opencode</span>
          <span className="text-text font-semibold">{version}</span>
        </div>
      )}
      <div className="flex items-center gap-1.5">
        <span className="text-muted">pending</span>
        <span className="text-text font-semibold">{pendingCount}</span>
      </div>
    </div>
  );
}

// ── Page ────────────────────────────────────────────────────────────────

export default function AnalystTeam() {
  const { activities, snapshot, status, paused, setPaused, clear } = useAnalystStream({
    initialLimit: 500,
  });
  const [analystStatus, setAnalystStatus] = useState<AnalystStatus | null>(null);
  useEffect(() => {
    let alive = true;
    const tick = () =>
      fetchAnalystStatus()
        .then((s) => {
          if (alive) setAnalystStatus(s);
        })
        .catch(() => {});
    tick();
    const id = setInterval(tick, 10_000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);

  const [question, setQuestion] = useState('');
  const [topic, setTopic] = useState('general');
  const [urgent, setUrgent] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [lastJob, setLastJob] = useState<string | null>(null);
  const [submitErr, setSubmitErr] = useState<string | null>(null);
  // When non-null, the feed filters to one run.
  const [selectedRun, setSelectedRun] = useState<string | null>(null);

  const runs = useMemo(() => deriveRuns(activities), [activities]);
  const grouped = useMemo(() => groupByRun(activities), [activities]);
  const systemEvents = useMemo(
    () => activities.filter((a) => !a.run_id),
    [activities],
  );

  // Bubbles list — newest at top so a new query appears immediately.
  const visibleBubbles = useMemo(() => {
    const withRun = grouped.filter((g) => g.run_id !== '');
    const filtered = selectedRun
      ? withRun.filter((g) => g.run_id === selectedRun)
      : withRun;
    return filtered
      .map((g) => {
        const run = runs.find((r) => r.run_id === g.run_id);
        if (!run) return null;
        return { run, items: g.items };
      })
      .filter((x): x is { run: AnalystRun; items: AnalystActivity[] } => x !== null)
      .sort((a, b) => new Date(b.run.started_at).getTime() - new Date(a.run.started_at).getTime());
  }, [grouped, runs, selectedRun]);

  // If the selected run drops out of view (e.g. ring buffer trimmed it),
  // fall back to "All".
  useEffect(() => {
    if (selectedRun && !runs.some((r) => r.run_id === selectedRun)) {
      setSelectedRun(null);
    }
  }, [runs, selectedRun]);

  // Auto-scroll to top when a new run appears (since newest is at top).
  const feedRef = useRef<HTMLDivElement>(null);
  const lastRunCount = useRef(visibleBubbles.length);
  useEffect(() => {
    if (!feedRef.current || paused) return;
    if (visibleBubbles.length > lastRunCount.current) {
      feedRef.current.scrollTop = 0;
    }
    lastRunCount.current = visibleBubbles.length;
  }, [visibleBubbles.length, paused]);

  const send = async () => {
    const q = question.trim();
    if (!q) return;
    setSubmitting(true);
    setSubmitErr(null);
    try {
      const r = await askAnalyst(q, topic.trim() || 'general', urgent);
      if ('error' in r) {
        setSubmitErr(r.error);
      } else {
        setLastJob(r.job_id);
        setQuestion('');
      }
    } catch (e) {
      setSubmitErr(String(e));
    } finally {
      setSubmitting(false);
    }
  };

  const enabled = snapshot?.enabled ?? false;
  const ready = analystStatus?.ready ?? snapshot?.ready ?? false;
  const pending = snapshot?.pending_jobs ?? [];

  return (
    <div className="h-full flex flex-col">
      {/* Header */}
      <div className="px-6 py-4 border-b border-border bg-panel">
        <div className="flex items-center justify-between mb-2">
          <div>
            <h1 className="text-xl font-bold text-text">Data Analyst</h1>
            <p className="text-[12px] text-muted mt-0.5">
              opencode agent — F1 pit-wall analyst. Each query, lap completion,
              and significant event becomes its own message below.
            </p>
          </div>
          <StatusBadge
            ready={ready}
            version={analystStatus?.version}
            pendingCount={pending.length}
            status={status}
          />
        </div>

        {!enabled || !ready ? (
          <div className="mt-2 px-3 py-2 rounded bg-bg border border-border/60 text-[12px] text-muted">
            {!enabled ? (
              <>
                Data Analyst is disabled. Set <code className="text-accent">DA_ENABLED=true</code>{' '}
                in Settings, supply <code className="text-accent">DA_PROVIDER</code> (and the
                relevant API key for that provider), and restart the server.
              </>
            ) : (
              <>
                Data Analyst is starting up. opencode subprocess hasn't
                completed initialize+session/new yet.{' '}
                {analystStatus?.reason && <span className="text-amber-400">{analystStatus.reason}</span>}
              </>
            )}
          </div>
        ) : null}
      </div>

      {/* Ask box */}
      <div className="px-6 py-3 border-b border-border bg-bg">
        <div className="flex gap-2 items-start">
          <div className="flex-1">
            <textarea
              className="w-full bg-panel border border-border rounded px-3 py-2 text-[13px] text-text resize-none focus:outline-none focus:border-accent"
              rows={2}
              placeholder="Ask the Data Analyst — e.g. 'Should I undercut Hamilton next lap?' (Cmd/Ctrl+Enter to send)"
              value={question}
              onChange={(e) => setQuestion(e.target.value)}
              onKeyDown={(e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
                  e.preventDefault();
                  send();
                }
              }}
              disabled={submitting}
            />
            <div className="flex items-center gap-3 mt-1.5 text-[11px] text-muted">
              <label className="flex items-center gap-1.5">
                <span>topic</span>
                <input
                  className="bg-panel border border-border rounded px-2 py-0.5 text-[11px] text-text w-[120px]"
                  value={topic}
                  onChange={(e) => setTopic(e.target.value)}
                />
              </label>
              <label className="flex items-center gap-1.5 cursor-pointer">
                <input type="checkbox" checked={urgent} onChange={(e) => setUrgent(e.target.checked)} />
                <span>urgent (also broadcasts on team radio)</span>
              </label>
              {lastJob && (
                <span className="ml-auto text-accent">job: {lastJob}</span>
              )}
              {submitErr && (
                <span className="ml-auto text-red-400">{submitErr}</span>
              )}
            </div>
          </div>
          <button
            onClick={send}
            disabled={submitting || !question.trim()}
            className="px-4 py-2 bg-accent text-bg font-semibold rounded text-[13px] hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed h-fit"
          >
            {submitting ? 'Sending…' : 'Ask'}
          </button>
        </div>
      </div>

      {/* Pending jobs strip */}
      {pending.length > 0 && (
        <div className="px-6 py-2 border-b border-border bg-panel/50">
          <div className="text-[10px] uppercase tracking-wider text-muted mb-1">In flight</div>
          <div className="flex gap-2 flex-wrap">
            {pending.map((j) => (
              <button
                key={j.job_id}
                type="button"
                onClick={() => j.run_id && setSelectedRun(j.run_id)}
                className="px-2 py-1 rounded bg-bg border border-border/60 text-[11px] hover:border-accent transition-colors text-left"
                title={j.question}
              >
                <span className="text-accent font-mono mr-1.5">{j.job_id}</span>
                <span className="text-muted">{j.context_topic}</span>
                <span className="text-text ml-1.5">
                  {j.question.length > 60 ? j.question.slice(0, 60) + '…' : j.question}
                </span>
                {j.urgent && <span className="ml-1.5 text-red-400">URGENT</span>}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Toolbar */}
      <div className="px-6 py-2 border-b border-border bg-panel/30 flex items-center gap-3 text-[11px]">
        <button
          onClick={() => setPaused(!paused)}
          className={`px-2.5 py-1 rounded border ${
            paused
              ? 'bg-amber-900/40 border-amber-600 text-amber-300'
              : 'bg-bg border-border text-muted hover:text-text'
          }`}
        >
          {paused ? 'Resume' : 'Pause'}
        </button>
        <button
          onClick={clear}
          className="px-2.5 py-1 rounded border border-border bg-bg text-muted hover:text-text"
        >
          Clear
        </button>
        {selectedRun && (
          <button
            onClick={() => setSelectedRun(null)}
            className="px-2.5 py-1 rounded border border-accent/60 bg-accent/15 text-accent hover:bg-accent/25"
          >
            ← Back to all runs
          </button>
        )}
        <span className="text-muted ml-auto">
          {selectedRun
            ? `1 run · ${visibleBubbles[0]?.items.length ?? 0} steps`
            : `${visibleBubbles.length} runs · ${activities.length} activities`}
          {analystStatus?.session_id && ` · session ${analystStatus.session_id}`}
        </span>
      </div>

      {/* Split: runs sidebar (left) + bubble feed (right) */}
      <div className="flex-1 flex min-h-0">
        {/* Runs sidebar */}
        <div className="w-[280px] shrink-0 border-r border-border bg-panel/40 overflow-y-auto">
          <div className="px-3 py-2 text-[10px] uppercase tracking-wider text-muted border-b border-border/60">
            Runs · click to follow one
          </div>
          <button
            type="button"
            onClick={() => setSelectedRun(null)}
            className={`w-full text-left px-3 py-2 border-b border-border/40 transition-colors ${
              selectedRun === null ? 'bg-accent/15 border-l-2 border-l-accent' : 'hover:bg-bg/50'
            }`}
          >
            <div className="text-[12px] font-semibold text-text">All runs</div>
            <div className="text-[11px] text-muted mt-0.5">
              {runs.length === 0 ? 'No runs yet' : `${runs.length} runs`}
            </div>
          </button>
          {runs.length === 0 ? (
            <div className="px-3 py-4 text-[11px] text-muted">
              No runs yet. Each query / lap-complete / significant-event spawns one.
            </div>
          ) : (
            runs.map((r) => (
              <RunCard
                key={r.run_id}
                run={r}
                selected={selectedRun === r.run_id}
                onClick={() => setSelectedRun(r.run_id)}
              />
            ))
          )}
        </div>

        {/* Bubble feed */}
        <div ref={feedRef} className="flex-1 overflow-y-auto bg-bg min-w-0">
          <div className="px-6 py-4 space-y-3">
            {!selectedRun && systemEvents.length > 0 && (
              <SystemEventStrip items={systemEvents} />
            )}
            {visibleBubbles.length === 0 ? (
              <div className="p-8 text-center text-muted text-[13px]">
                {selectedRun
                  ? `No activity recorded for ${selectedRun} (yet).`
                  : enabled
                  ? 'Waiting for analyst activity… the first trigger will appear here.'
                  : 'Data Analyst is not running. The feed will start streaming as soon as it does.'}
              </div>
            ) : (
              visibleBubbles.map(({ run, items }, i) => (
                <RunBubble
                  key={run.run_id}
                  run={run}
                  items={items}
                  expandedDefault={i === 0 || !!selectedRun}
                />
              ))
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
