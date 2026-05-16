import { useEffect, useState } from 'react';
import { API_BASE } from '../lib/constants';

type GateState =
  | { status: 'waiting'; attempts: number }
  | { status: 'ready' }
  | { status: 'unreachable'; attempts: number; lastError: string };

const POLL_INTERVAL_MS = 250;
const SOFT_LIMIT_MS = 15000; // surface a "still trying" message after 15s
const HARD_LIMIT_MS = 60000; // show a retry button after 60s

/**
 * ServerGate blocks dashboard rendering until the embedded telemetry-core
 * answers /health. In the Wails .app this only matters in the first ~2s
 * after launch (the Go shim waits for /health in OnStartup, but if the
 * server crashes mid-session or in dev the gate also covers that gap).
 *
 * Renders a branded splash with a soft progress hint, then unmounts and
 * passes children through once the server is reachable. Children are
 * NOT mounted prematurely, so every downstream fetch hook is guaranteed
 * a live backend — no per-hook retry boilerplate needed.
 */
export function ServerGate({ children }: { children: React.ReactNode }) {
  const [state, setState] = useState<GateState>({ status: 'waiting', attempts: 0 });
  const [startedAt] = useState(() => Date.now());

  useEffect(() => {
    let cancelled = false;
    let timer: number | null = null;

    const poll = async () => {
      try {
        const r = await fetch(`${API_BASE}/health`, { cache: 'no-store' });
        if (cancelled) return;
        if (r.ok) {
          setState({ status: 'ready' });
          return;
        }
        throw new Error(`HTTP ${r.status}`);
      } catch (e) {
        if (cancelled) return;
        setState((prev) => ({
          status: Date.now() - startedAt > HARD_LIMIT_MS ? 'unreachable' : 'waiting',
          attempts: (prev.status === 'ready' ? 0 : prev.attempts) + 1,
          lastError: e instanceof Error ? e.message : String(e),
        }));
        timer = window.setTimeout(poll, POLL_INTERVAL_MS);
      }
    };

    poll();

    return () => {
      cancelled = true;
      if (timer !== null) window.clearTimeout(timer);
    };
  }, [startedAt]);

  if (state.status === 'ready') return <>{children}</>;

  const elapsed = Date.now() - startedAt;
  const showStillTrying = elapsed > SOFT_LIMIT_MS;
  const unreachable = state.status === 'unreachable';

  return (
    <div className="h-screen w-screen bg-bg text-text flex items-center justify-center">
      <div className="flex flex-col items-center gap-6 max-w-md text-center px-6">
        <div className="flex items-center gap-3">
          <Spinner />
          <div className="text-left">
            <div className="text-white text-lg font-bold leading-tight">Race Engineer</div>
            <div className="text-[11px] text-muted uppercase tracking-wider">Pit Wall</div>
          </div>
        </div>

        <div className="text-sm text-muted">
          {unreachable
            ? 'Telemetry core is not responding.'
            : showStillTrying
              ? 'Still starting the telemetry core…'
              : 'Starting the telemetry core…'}
        </div>

        {unreachable && (
          <button
            type="button"
            onClick={() => window.location.reload()}
            className="px-4 py-2 text-xs font-bold rounded-md border border-accent text-accent hover:bg-accent hover:text-bg transition-colors"
          >
            Retry
          </button>
        )}

        <div className="text-[10px] text-muted font-mono opacity-60">
          attempt {state.attempts}
          {state.status === 'unreachable' && state.attempts > 1 && (
            <> · {state.lastError}</>
          )}
        </div>
      </div>
    </div>
  );
}

function Spinner() {
  return (
    <span
      className="inline-block w-5 h-5 rounded-full border-2 border-border border-t-accent animate-spin"
      aria-hidden
    />
  );
}
