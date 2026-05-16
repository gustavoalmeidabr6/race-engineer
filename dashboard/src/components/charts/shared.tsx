import type { ReactNode } from 'react';

// shared.tsx — small chart primitives reused by both DeepInsights (post-race
// historical analysis) and the live Telemetry page. Single source of truth
// for card chrome, lap colours, channel catalogue, and the lap-time / delta
// formatters so the two surfaces stay visually consistent.

export const LAP_COLORS = [
  '#FFD700', // gold — reserved for "best" / reference
  '#58a6ff', // blue
  '#bc8cff', // purple
  '#2ea043', // green
  '#FF9300', // orange
];

export const ALL_CHANNELS = [
  { id: 'throttle', label: 'Throttle' },
  { id: 'brake', label: 'Brake' },
  { id: 'speed', label: 'Speed' },
  { id: 'gear', label: 'Gear' },
  { id: 'steering', label: 'Steering' },
  { id: 'rpm', label: 'RPM' },
  { id: 'g_lat', label: 'G Lateral' },
  { id: 'g_lon', label: 'G Long.' },
  { id: 'g_vert', label: 'G Vert.' },
  { id: 'brake_temp', label: 'Brake Temp' },
  { id: 'tyre_temp', label: 'Tyre Temp' },
  { id: 'tyre_inner_temp', label: 'Tyre Inner T' },
  { id: 'tyre_pressure', label: 'Tyre Pressure' },
  { id: 'fuel', label: 'Fuel' },
  { id: 'ers_store', label: 'ERS Store' },
  { id: 'clutch', label: 'Clutch' },
] as const;

export function CardWrap({
  title,
  children,
  className,
  right,
}: {
  title: string;
  children: ReactNode;
  className?: string;
  right?: ReactNode;
}) {
  return (
    <div
      className={`bg-panel border border-border rounded-lg p-3.5 flex flex-col overflow-hidden ${className ?? ''}`}
    >
      <div className="flex items-center justify-between mb-2.5">
        <h2 className="text-[13px] text-muted uppercase tracking-wider">{title}</h2>
        {right}
      </div>
      <div className="flex-1 min-h-0">{children}</div>
    </div>
  );
}

export function formatLapMs(ms: number | null | undefined): string {
  if (!ms || ms <= 0) return '—';
  const totalSec = ms / 1000;
  const m = Math.floor(totalSec / 60);
  const s = totalSec - m * 60;
  return `${m}:${s.toFixed(3).padStart(6, '0')}`;
}

export function formatDeltaMs(ms: number | undefined | null): string {
  if (ms == null || Number.isNaN(ms)) return '—';
  const sign = ms >= 0 ? '+' : '−';
  return `${sign}${(Math.abs(ms) / 1000).toFixed(3)}s`;
}

export function deltaClass(
  value: number | undefined | null,
  betterIfNegative = true,
): string {
  if (value == null) return 'text-muted';
  const better = betterIfNegative ? value < 0 : value > 0;
  if (Math.abs(value) < 0.05) return 'text-muted';
  return better ? 'text-success' : 'text-danger';
}
