import { createContext, useContext, useEffect, useRef, type ReactNode } from 'react';
import { useLiveSession, type UseLiveSessionResult } from '../hooks/useLiveSession';

/**
 * LiveSessionContext shares one useLiveSession instance across the app so
 * there's a single Gemini Live session for the entire dashboard lifetime,
 * not one per page that mounts the panel.
 *
 * The provider also handles auto-connect + auto-reconnect: it tries to
 * start a session on mount, and re-tries with a short backoff if the
 * session drops (network blip, server restart, transient error). The
 * old manual "Start session" button is gone; the dashboard just shows a
 * status chip in the Sidebar and the user can talk whenever it's green.
 */
const LiveSessionContext = createContext<UseLiveSessionResult | null>(null);

const RECONNECT_DELAY_MS = 3000;

export function LiveSessionProvider({ children }: { children: ReactNode }) {
  const session = useLiveSession();
  const reconnectTimerRef = useRef<number | null>(null);

  // Auto-start once on mount, and auto-reconnect whenever the session
  // returns to idle or error. We deliberately depend on session.state
  // (not just mount) so a deliberate stop() OR a network-driven drop
  // both trigger another connect attempt after the backoff window.
  //
  // Notes:
  //   - useLiveSession.start() is gated by an internal ref-based mutex,
  //     so even if React StrictMode double-fires this effect in dev the
  //     second call is a no-op.
  //   - getUserMedia may fail on first load if the browser hasn't been
  //     granted mic permission. The session enters 'error', the user
  //     sees the red status chip; clicking it triggers another start
  //     which re-prompts.
  useEffect(() => {
    if (session.state === 'ready' || session.state === 'connecting' || session.state === 'requesting-mic') {
      return;
    }
    // 'superseded' means another tab owns the single Gemini Live slot.
    // Auto-reconnecting would kick that tab out and the two would
    // oscillate forever, each shouting the other down. Stay idle until
    // the user explicitly clicks Start in this tab (which calls
    // session.start() and re-enters the connect flow).
    if (session.state === 'superseded') {
      if (reconnectTimerRef.current != null) {
        window.clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
      return;
    }
    if (reconnectTimerRef.current != null) {
      window.clearTimeout(reconnectTimerRef.current);
    }
    // First attempt fires almost immediately on mount; subsequent
    // reconnects wait RECONNECT_DELAY_MS to avoid hammering the API on
    // a hard server failure.
    const delay = session.state === 'idle' && !session.error ? 100 : RECONNECT_DELAY_MS;
    reconnectTimerRef.current = window.setTimeout(() => {
      void session.start();
    }, delay);
    return () => {
      if (reconnectTimerRef.current != null) {
        window.clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
    };
  }, [session.state, session.error, session.start]);

  return (
    <LiveSessionContext.Provider value={session}>
      {children}
    </LiveSessionContext.Provider>
  );
}

export function useSharedLiveSession(): UseLiveSessionResult {
  const ctx = useContext(LiveSessionContext);
  if (!ctx) {
    throw new Error('useSharedLiveSession must be used inside <LiveSessionProvider>');
  }
  return ctx;
}
