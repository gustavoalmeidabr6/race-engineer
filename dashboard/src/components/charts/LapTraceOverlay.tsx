import { useMemo } from 'react';
import {
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import type { TraceLap, TracesResponse } from '../../types/sessions';
import { LAP_COLORS } from './shared';

// traceKey uniquely identifies a lap in the response: same lap number can
// appear twice when the picker stacks the current session alongside a past
// one, so we suffix with the session_uid tail when present.
export function traceKey(lap: TraceLap): string {
  return lap.session_uid ? `${lap.lap}@${lap.session_uid}` : `${lap.lap}`;
}

// LapTraceOverlay renders one or more laps' channel arrays on the same
// distance axis. Used by:
//   - Deep Insights "Lap Trace Overlay" card (best vs one selected lap)
//   - Telemetry → Compare tab (best vs up to 3 picked laps)
// The component is pure: pass the /api/laps/traces response and the list
// of active channel ids, get back a recharts <LineChart>. Lap-to-colour
// mapping follows the order in `data.laps`, so callers should put the
// reference lap first to keep gold for "best".

interface Props {
  data: TracesResponse | null;
  activeChannels: string[];
  height?: string | number;
  isAnimationActive?: boolean;
}

export function LapTraceOverlay({
  data,
  activeChannels,
  height = '100%',
  isAnimationActive = false,
}: Props) {
  const chartData = useMemo(
    () => buildTraceChartData(data, activeChannels),
    [data, activeChannels],
  );

  if (!data || data.laps.length === 0) {
    return (
      <div className="h-full flex items-center justify-center text-xs text-muted">
        No hi-freq data captured for this lap
      </div>
    );
  }

  return (
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={chartData} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
        <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
        <XAxis
          dataKey="bucket_m"
          tick={{ fill: '#8b949e', fontSize: 10 }}
          stroke="#30363d"
          tickFormatter={(v) => `${(v / 1000).toFixed(1)}km`}
        />
        <YAxis tick={{ fill: '#8b949e', fontSize: 10 }} stroke="#30363d" />
        <Tooltip
          contentStyle={{
            background: '#161b22',
            border: '1px solid #30363d',
            borderRadius: '6px',
            fontSize: '11px',
          }}
          labelStyle={{ color: '#c9d1d9' }}
          labelFormatter={(v) => `${(v / 1000).toFixed(2)} km`}
        />
        <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
        {data.laps.flatMap((trace, lapIdx) => {
          const key = traceKey(trace);
          const sessionTag = trace.session_uid ? ` · S${trace.session_uid.slice(-4)}` : '';
          return activeChannels.map((channel) => (
            <Line
              key={`${key}-${channel}`}
              type="monotone"
              dataKey={`${channel}_${key}`}
              name={`L${trace.lap}${sessionTag} ${channel}`}
              stroke={LAP_COLORS[lapIdx % LAP_COLORS.length]}
              strokeWidth={lapIdx === 0 ? 2 : 1.5}
              strokeDasharray={lapIdx === 0 ? '0' : '4 2'}
              dot={false}
              isAnimationActive={isAnimationActive}
            />
          ));
        })}
      </LineChart>
    </ResponsiveContainer>
  );
}

export function buildTraceChartData(
  data: TracesResponse | null,
  channels: string[],
) {
  if (!data || data.laps.length === 0) return [];
  const buckets = data.laps[0].track_position_bucket_m;
  return buckets.map((bucket_m, i) => {
    const row: Record<string, number | null> = { bucket_m };
    for (const lap of data.laps) {
      const key = traceKey(lap);
      for (const channel of channels) {
        row[`${channel}_${key}`] = lap.channels[channel]?.[i] ?? null;
      }
    }
    return row;
  });
}
