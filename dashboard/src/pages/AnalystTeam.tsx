import { useMemo, useRef, useState, useEffect } from 'react';
import {
  usePiAgentStream,
  askAnalystTeam,
  deriveRuns,
  type PiActivity,
  type PiRun,
} from '../hooks/usePiAgentStream';

// Colour palette per activity kind. Keeps the timeline readable at a glance.
const KIND_COLORS: Record<string, { bg: string; fg: string; label: string }> = {
  trigger:        { bg: '#1e2a4a', fg: '#7aa2ff', label: 'TRIGGER' },
  tool_call:      { bg: '#1f1f2e', fg: '#9b9bd6', label: 'TOOL' },
  tool_result:    { bg: '#1a221a', fg: '#7fc97f', label: 'RESULT' },
  observation:    { bg: '#1f2620', fg: '#a8d4a8', label: 'OBSERVATION' },
  insight_pushed: { bg: '#2a1f1f', fg: '#ff9b6a', label: 'RADIO →' },
  query_answer:   { bg: '#2a261f', fg: '#f0c674', label: 'ANSWER' },
  error:          { bg: '#2a1818', fg: '#ff7070', label: 'ERROR' },
};

function kindStyle(k: string) {
  return KIND_COLORS[k] ?? { bg: '#1f1f1f', fg: '#aaaaaa', label: k.toUpperCase() };
}

function fmtTime(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleTimeString('en-GB', { hour12: false }) + '.' +
      String(d.getMilliseconds()).padStart(3, '0');
  } catch {
    return iso;
  }
}

function ActivityRow({ a, showRunBadge }: { a: PiActivity; showRunBadge?: boolean }) {
  const s = kindStyle(a.kind);
  const subtitle = useMemo(() => {
    if (a.tool && a.kind === 'tool_call') return a.tool;
    if (a.tool && a.kind === 'tool_result') return a.tool + (a.duration_ms ? ` · ${a.duration_ms}ms` : '');
    if (a.kind === 'trigger' && a.meta?.kind) return String(a.meta.kind);
    if (a.kind === 'insight_pushed' && a.meta?.priority) return `priority ${a.meta.priority}`;
    if (a.kind === 'observation' && a.meta?.topic) return String(a.meta.topic);
    if (a.kind === 'query_answer' && a.meta?.job_id) return String(a.meta.job_id);
    return '';
  }, [a]);

  return (
    <div className="flex gap-3 px-4 py-2 border-b border-border/40 hover:bg-bg/40 transition-colors">
      <div className="text-[10px] font-mono text-muted whitespace-nowrap pt-0.5 w-[88px] shrink-0">
        {fmtTime(a.at)}
      </div>
      <div
        className="text-[10px] font-bold uppercase tracking-wider px-2 py-0.5 rounded h-fit shrink-0"
        style={{ backgroundColor: s.bg, color: s.fg }}
      >
        {s.label}
      </div>
      {showRunBadge && a.run_id && (
        <div className="text-[10px] font-mono text-accent/80 whitespace-nowrap pt-0.5 shrink-0">
          {a.run_id}
        </div>
      )}
      <div className="flex-1 min-w-0">
        {subtitle && (
          <div className="text-[11px] text-accent font-mono mb-0.5 truncate">{subtitle}</div>
        )}
        <div className="text-[13px] text-text leading-snug whitespace-pre-wrap break-words">
          {a.summary || (a.error ? `error: ${a.error}` : '(no summary)')}
        </div>
        {a.kind === 'tool_call' && a.args && Object.keys(a.args).length > 0 && (
          <div className="text-[11px] text-muted font-mono mt-1 truncate">
            {JSON.stringify(a.args)}
          </div>
        )}
      </div>
    </div>
  );
}

// TerminalLine renders one activity as a single dense terminal-style row.
// Used in the per-run view to look like a tail of pi-agent's stdout.
function TerminalLine({ a }: { a: PiActivity }) {
  const s = kindStyle(a.kind);
  const lhs =
    a.kind === 'tool_call'
      ? `→ ${a.tool ?? '?'}`
      : a.kind === 'tool_result'
      ? `← ${a.tool ?? '?'}`
      : a.kind === 'trigger'
      ? `trigger ${(a.meta?.kind as string) ?? ''}`
      : a.kind === 'observation'
      ? `obs ${(a.meta?.topic as string) ?? ''}`
      : a.kind === 'insight_pushed'
      ? `radio P${(a.meta?.priority as number) ?? '?'}`
      : a.kind === 'query_answer'
      ? `answer ${(a.meta?.job_id as string) ?? ''}`
      : a.kind === 'error'
      ? 'ERR'
      : a.kind;
  const dur = a.duration_ms ? ` (${a.duration_ms}ms)` : '';
  const body =
    a.summary ||
    (a.kind === 'tool_call' && a.args ? JSON.stringify(a.args) : '') ||
    (a.error ?? '');
  return (
    <div className="flex gap-2 px-3 py-0.5 font-mono text-[12px] leading-snug hover:bg-bg/60">
      <span className="text-muted whitespace-nowrap">{fmtTime(a.at)}</span>
      <span style={{ color: s.fg }} className="whitespace-nowrap shrink-0">
        {lhs}
        {dur}
      </span>
      <span className="text-text/90 break-words whitespace-pre-wrap min-w-0 flex-1">{body}</span>
    </div>
  );
}

// RunCard is one row in the left sidebar's runs list.
function RunCard({
  run,
  selected,
  onClick,
}: {
  run: PiRun;
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
  return (
    <button
      type="button"
      onClick={onClick}
      className={`w-full text-left px-3 py-2 border-b border-border/40 transition-colors ${
        selected ? 'bg-accent/15 border-l-2 border-l-accent' : 'hover:bg-bg/50'
      }`}
    >
      <div className="flex items-center gap-2">
        <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ backgroundColor: dot }} />
        <span className="text-[11px] font-mono text-accent truncate">{run.run_id}</span>
        <span className="ml-auto text-[10px] text-muted whitespace-nowrap">
          {run.activity_count}
        </span>
      </div>
      <div className="flex items-center gap-1.5 mt-1 text-[11px]">
        <span className="text-text font-semibold">{run.persona || '—'}</span>
        {run.trigger_kind && <span className="text-muted">· {run.trigger_kind}</span>}
        <span className="text-muted ml-auto">{fmtTime(run.last_at)}</span>
      </div>
      {run.preview && (
        <div className="text-[11px] text-muted mt-1 line-clamp-2">{run.preview}</div>
      )}
    </button>
  );
}

function StatusBadge({
  enabled,
  mode,
  provider,
  pendingCount,
  status,
}: {
  enabled: boolean;
  mode?: string;
  provider?: string;
  pendingCount: number;
  status: string;
}) {
  const dot =
    status === 'open'
      ? '#2ea043'
      : status === 'error'
      ? '#f85149'
      : '#d29922';
  return (
    <div className="flex items-center gap-3 text-[12px]">
      <div className="flex items-center gap-1.5">
        <span className="w-2 h-2 rounded-full" style={{ backgroundColor: dot }} />
        <span className="text-muted">stream</span>
        <span className="text-text font-semibold">{status}</span>
      </div>
      <div className="flex items-center gap-1.5">
        <span className="text-muted">mode</span>
        <span className={`font-semibold ${enabled && mode === 'on' ? 'text-accent' : 'text-muted'}`}>
          {mode ?? '—'}
        </span>
      </div>
      <div className="flex items-center gap-1.5">
        <span className="text-muted">provider</span>
        <span className="text-text font-semibold">{provider ?? '—'}</span>
      </div>
      <div className="flex items-center gap-1.5">
        <span className="text-muted">pending</span>
        <span className="text-text font-semibold">{pendingCount}</span>
      </div>
    </div>
  );
}

export default function AnalystTeam() {
  const { activities, snapshot, status, paused, setPaused, clear } = usePiAgentStream({
    initialLimit: 500,
  });
  const [question, setQuestion] = useState('');
  const [topic, setTopic] = useState('general');
  const [urgent, setUrgent] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [lastJob, setLastJob] = useState<string | null>(null);
  const [submitErr, setSubmitErr] = useState<string | null>(null);
  // When non-null, the main pane filters to one run and uses terminal styling.
  // null = "All" view = current behavior.
  const [selectedRun, setSelectedRun] = useState<string | null>(null);

  const runs = useMemo(() => deriveRuns(activities), [activities]);
  const filteredActivities = useMemo(
    () => (selectedRun ? activities.filter((a) => a.run_id === selectedRun) : activities),
    [activities, selectedRun],
  );

  // Auto-scroll to bottom on new activity unless paused.
  const feedRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!feedRef.current || paused) return;
    feedRef.current.scrollTop = feedRef.current.scrollHeight;
  }, [filteredActivities.length, paused]);

  // If the selected run drops out of view (e.g. ring buffer trimmed it),
  // fall back to "All".
  useEffect(() => {
    if (selectedRun && !runs.some((r) => r.run_id === selectedRun)) {
      setSelectedRun(null);
    }
  }, [runs, selectedRun]);

  const send = async () => {
    const q = question.trim();
    if (!q) return;
    setSubmitting(true);
    setSubmitErr(null);
    try {
      const r = await askAnalystTeam(q, topic.trim() || 'general', urgent);
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
  const mode = snapshot?.mode;
  const pending = snapshot?.pending_jobs ?? [];

  return (
    <div className="h-full flex flex-col">
      {/* Header */}
      <div className="px-6 py-4 border-b border-border bg-panel">
        <div className="flex items-center justify-between mb-2">
          <div>
            <h1 className="text-xl font-bold text-text">Analyst Team</h1>
            <p className="text-[12px] text-muted mt-0.5">
              Sandboxed pi-agent specialists. Every tool call streams here in
              real time.
            </p>
          </div>
          <StatusBadge
            enabled={enabled}
            mode={mode}
            provider={snapshot?.provider}
            pendingCount={pending.length}
            status={status}
          />
        </div>

        {!enabled || mode !== 'on' ? (
          <div className="mt-2 px-3 py-2 rounded bg-bg border border-border/60 text-[12px] text-muted">
            Pi agent is <span className="text-text font-semibold">{mode ?? 'off'}</span>.
            Set <code className="text-accent">PI_AGENT_MODE=on</code> in Settings (and an API
            key for the chosen provider) and restart the server. The
            activity feed below works the moment the agent starts running.
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
              placeholder="Ask the analyst team — e.g. 'Should I undercut Hamilton next lap?' (Cmd/Ctrl+Enter to send)"
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
              <div
                key={j.job_id}
                className="px-2 py-1 rounded bg-bg border border-border/60 text-[11px]"
                title={j.question}
              >
                <span className="text-accent font-mono mr-1.5">{j.job_id}</span>
                <span className="text-muted">{j.context_topic}</span>
                <span className="text-text ml-1.5">
                  {j.question.length > 60 ? j.question.slice(0, 60) + '…' : j.question}
                </span>
                {j.urgent && <span className="ml-1.5 text-red-400">URGENT</span>}
              </div>
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
            ? `${filteredActivities.length} entries in ${selectedRun}`
            : `${activities.length} activities · ${runs.length} runs`}
          {' · trigger queue depth '}{snapshot?.trigger_depth ?? 0}
        </span>
      </div>

      {/* Split: runs sidebar (left) + activity feed (right) */}
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
              Merged timeline · {activities.length} activities
            </div>
          </button>
          {runs.length === 0 ? (
            <div className="px-3 py-4 text-[11px] text-muted">
              No specialist runs yet. Each query/lap/event spawns one.
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

        {/* Activity feed (terminal mode when a run is selected) */}
        <div ref={feedRef} className="flex-1 overflow-y-auto bg-bg min-w-0">
          {filteredActivities.length === 0 ? (
            <div className="p-8 text-center text-muted text-[13px]">
              {selectedRun
                ? `No activity recorded for ${selectedRun} (yet).`
                : enabled
                ? 'Waiting for pi-agent activity… first trigger will appear here.'
                : 'Pi agent is not running. The feed will start streaming as soon as it does.'}
            </div>
          ) : selectedRun ? (
            <div className="py-1 font-mono">
              {filteredActivities.map((a) => <TerminalLine key={a.id} a={a} />)}
            </div>
          ) : (
            filteredActivities.map((a) => <ActivityRow key={a.id} a={a} showRunBadge />)
          )}
        </div>
      </div>
    </div>
  );
}
