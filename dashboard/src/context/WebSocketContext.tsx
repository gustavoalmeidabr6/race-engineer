import { createContext, useContext, type ReactNode } from 'react';
import { useWebSocket } from '../hooks/useWebSocket';
import type { RaceState } from '../types/telemetry';
import type { HealthStatus } from '../types/settings';
import type { TrackPositionDynamic } from '../types/trackPosition';

interface WebSocketContextValue {
  state: RaceState | null;
  connected: boolean;
  health: HealthStatus | null;
  trackPos: TrackPositionDynamic | null;
}

const WebSocketContext = createContext<WebSocketContextValue | null>(null);

export function WebSocketProvider({ children }: { children: ReactNode }) {
  const value = useWebSocket();
  return (
    <WebSocketContext.Provider value={value}>{children}</WebSocketContext.Provider>
  );
}

export function useTelemetryStream(): WebSocketContextValue {
  const ctx = useContext(WebSocketContext);
  if (!ctx) {
    throw new Error('useTelemetryStream must be used within a WebSocketProvider');
  }
  return ctx;
}
