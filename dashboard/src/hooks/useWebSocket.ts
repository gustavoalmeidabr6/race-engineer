import { useState, useEffect, useRef } from 'react';
import type { RaceState } from '../types/telemetry';
import type { HealthStatus } from '../types/settings';
import type { TrackPositionDynamic } from '../types/trackPosition';
import { API_BASE } from '../lib/constants';

function getWsUrl(): string {
  // API_BASE is absolute (http://…) only in prod (Wails .app); rewrite to ws(s).
  if (API_BASE) return API_BASE.replace(/^http/, 'ws') + '/ws';
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}

export function useWebSocket() {
  const [state, setState] = useState<RaceState | null>(null);
  const [connected, setConnected] = useState(false);
  const [health, setHealth] = useState<HealthStatus | null>(null);
  const [trackPos, setTrackPos] = useState<TrackPositionDynamic | null>(null);

  const wsRef = useRef<WebSocket | null>(null);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const backoff = useRef(1000);

  useEffect(() => {
    let unmounted = false;

    function connect() {
      if (unmounted) return;

      const ws = new WebSocket(getWsUrl());
      wsRef.current = ws;

      ws.onopen = () => {
        if (unmounted) return;
        setConnected(true);
        backoff.current = 1000;
      };

      ws.onclose = () => {
        if (unmounted) return;
        setConnected(false);
        wsRef.current = null;
        // Exponential backoff reconnect: 1s -> 2s -> 4s -> max 10s.
        reconnectTimer.current = setTimeout(() => {
          backoff.current = Math.min(backoff.current * 2, 10000);
          connect();
        }, backoff.current);
      };

      ws.onerror = () => {
        // onclose will fire after onerror, triggering reconnect.
        ws.close();
      };

      ws.onmessage = (event) => {
        if (unmounted) return;
        try {
          const msg = JSON.parse(event.data) as { type: string; data: unknown };
          switch (msg.type) {
            case 'telemetry':
              setState(msg.data as RaceState);
              break;
            case 'health':
              setHealth(msg.data as HealthStatus);
              break;
            case 'track_position':
              setTrackPos(msg.data as TrackPositionDynamic);
              break;
          }
          // 'insight', 'audio', 'ptt' messages are intentionally ignored —
          // engineer voice and PTT live in the Gemini Live service, not the
          // dashboard. Dashboard is telemetry-only.
        } catch {
          // Ignore malformed messages.
        }
      };
    }

    connect();

    return () => {
      unmounted = true;
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      if (wsRef.current) {
        wsRef.current.close();
      }
    };
  }, []);

  return { state, connected, health, trackPos };
}
