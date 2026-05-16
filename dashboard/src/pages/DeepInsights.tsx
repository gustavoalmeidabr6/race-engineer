import { useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
  Legend,
} from 'recharts';
import { LapTraceOverlay } from '../components/charts/LapTraceOverlay';
import {
  useLapCompare,
  useLapTraces,
  useSessionSummary,
  useSessionsList,
} from '../hooks/useSessions';
import type {
  CornerCompare,
  LapSummary,
  SessionSummary,
  EventSummary,
} from '../types/sessions';
import {
  ALL_CHANNELS,
  CardWrap,
  deltaClass,
  formatDeltaMs,
  formatLapMs,
} from '../components/charts/shared';

function formatDate(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    dateStyle: 'short',
    timeStyle: 'short',
  });
}

function LapTimeChart({ summary }: { summary: SessionSummary }) {
  const data = summary.laps.map((l) => ({
    lap: l.lap,
    time_s: l.lap_time_ms / 1000,
    valid: l.valid,
  }));
  const bestLap = summary.best_lap_ms
    ? summary.laps.find((l) => l.lap_time_ms === summary.best_lap_ms)?.lap
    : null;

  return (
    <CardWrap title={`Lap Times (best ${formatLapMs(summary.best_lap_ms)})`} className="h-[260px]">
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={data} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis dataKey="lap" tick={{ fill: '#8b949e', fontSize: 11 }} stroke="#30363d" />
          <YAxis
            tick={{ fill: '#8b949e', fontSize: 11 }}
            stroke="#30363d"
            domain={['auto', 'auto']}
            tickFormatter={(v) => v.toFixed(1)}
          />
          <Tooltip
            contentStyle={{
              background: '#161b22',
              border: '1px solid #30363d',
              borderRadius: '6px',
              fontSize: '12px',
            }}
            labelStyle={{ color: '#c9d1d9' }}
            formatter={(v: number, _name, props) => [
              `${v.toFixed(3)}s${bestLap === props.payload.lap ? ' ⭐ best' : ''}`,
              'Lap',
            ]}
            labelFormatter={(lap) => `Lap ${lap}`}
          />
          <Line
            type="monotone"
            dataKey="time_s"
            stroke="#58a6ff"
            strokeWidth={2}
            dot={(p: { cx?: number; cy?: number; index?: number }) => {
              const idx = p.index ?? 0;
              const pt = data[idx];
              const isBest = pt && pt.lap === bestLap;
              const cx = p.cx ?? 0;
              const cy = p.cy ?? 0;
              return (
                <circle
                  key={`pt-${idx}`}
                  cx={cx}
                  cy={cy}
                  r={isBest ? 5 : 2.5}
                  fill={isBest ? '#FFD700' : '#58a6ff'}
                  stroke="#0d1117"
                  strokeWidth={1}
                />
              );
            }}
          />
        </LineChart>
      </ResponsiveContainer>
    </CardWrap>
  );
}

function SectorHeatmap({ laps, bestLapMs }: { laps: LapSummary[]; bestLapMs: number | null }) {
  // Personal-best per sector for colour reference.
  const pb = useMemo(() => {
    const out = { s1: Infinity, s2: Infinity, s3: Infinity };
    for (const l of laps) {
      if (l.sector1_ms > 0 && l.sector1_ms < out.s1) out.s1 = l.sector1_ms;
      if (l.sector2_ms > 0 && l.sector2_ms < out.s2) out.s2 = l.sector2_ms;
      if (l.sector3_ms > 0 && l.sector3_ms < out.s3) out.s3 = l.sector3_ms;
    }
    return out;
  }, [laps]);

  function color(value: number, best: number) {
    if (!value || best === Infinity || value <= 0) return 'transparent';
    const delta = value - best;
    if (delta <= 0) return '#bc8cff';
    if (delta < 250) return '#2ea04340';
    if (delta < 500) return '#d2992240';
    return '#f8514940';
  }

  return (
    <CardWrap title={`Sectors (PB s1/s2/s3 = ${formatLapMs(pb.s1)} ${formatLapMs(pb.s2)} ${formatLapMs(pb.s3)})`}>
      <div className="overflow-y-auto h-full">
        <table className="w-full text-xs">
          <thead className="text-[10px] text-muted uppercase tracking-wider">
            <tr>
              <th className="text-left font-normal pb-1">Lap</th>
              <th className="text-right font-normal pb-1">S1</th>
              <th className="text-right font-normal pb-1">S2</th>
              <th className="text-right font-normal pb-1">S3</th>
              <th className="text-right font-normal pb-1">Total</th>
            </tr>
          </thead>
          <tbody>
            {laps.map((l) => (
              <tr key={l.lap} className="border-t border-border">
                <td className="py-1 text-text">
                  {l.lap}
                  {bestLapMs && l.lap_time_ms === bestLapMs && (
                    <span className="ml-1 text-warning">★</span>
                  )}
                </td>
                <td className="py-1 text-right tabular-nums" style={{ background: color(l.sector1_ms, pb.s1) }}>
                  {(l.sector1_ms / 1000).toFixed(3)}
                </td>
                <td className="py-1 text-right tabular-nums" style={{ background: color(l.sector2_ms, pb.s2) }}>
                  {(l.sector2_ms / 1000).toFixed(3)}
                </td>
                <td className="py-1 text-right tabular-nums" style={{ background: color(l.sector3_ms, pb.s3) }}>
                  {(l.sector3_ms / 1000).toFixed(3)}
                </td>
                <td className="py-1 text-right tabular-nums text-accent">
                  {formatLapMs(l.lap_time_ms)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </CardWrap>
  );
}

function LapTraceCard({
  uid,
  laps,
  bestLap,
}: {
  uid: string;
  laps: LapSummary[];
  bestLap: number | null;
}) {
  const [activeChannels, setActiveChannels] = useState<string[]>(['throttle', 'brake', 'speed']);
  const [selectedLap, setSelectedLap] = useState<number | null>(bestLap);

  useEffect(() => {
    setSelectedLap(bestLap);
  }, [bestLap]);

  const lapsParam = useMemo(() => {
    if (selectedLap && bestLap && selectedLap !== bestLap) {
      return `${bestLap},${selectedLap}`;
    }
    if (bestLap) return `${bestLap}`;
    return 'last';
  }, [selectedLap, bestLap]);

  const { data, loading } = useLapTraces(uid, {
    laps: lapsParam,
    channels: activeChannels.join(','),
    buckets: 80,
  });

  function toggleChannel(id: string) {
    setActiveChannels((prev) =>
      prev.includes(id) ? prev.filter((p) => p !== id) : [...prev, id],
    );
  }

  return (
    <CardWrap title="Lap Trace Overlay" className="h-[420px]">
      <div className="flex flex-wrap gap-1.5 mb-2">
        {ALL_CHANNELS.map((ch) => {
          const active = activeChannels.includes(ch.id);
          return (
            <button
              key={ch.id}
              onClick={() => toggleChannel(ch.id)}
              className={`text-[11px] px-2 py-0.5 rounded border transition-colors ${
                active
                  ? 'bg-accent/20 border-accent text-accent'
                  : 'border-border text-muted hover:text-white'
              }`}
            >
              {ch.label}
            </button>
          );
        })}
      </div>
      <div className="flex flex-wrap gap-1.5 mb-2">
        <span className="text-[11px] text-muted self-center">Lap:</span>
        {bestLap && (
          <button
            onClick={() => setSelectedLap(bestLap)}
            className={`text-[11px] px-2 py-0.5 rounded border ${
              selectedLap === bestLap
                ? 'bg-warning/20 border-warning text-warning'
                : 'border-border text-muted hover:text-white'
            }`}
          >
            Best (L{bestLap})
          </button>
        )}
        {laps.slice(-8).map((l) => {
          if (l.lap === bestLap) return null;
          return (
            <button
              key={l.lap}
              onClick={() => setSelectedLap(l.lap)}
              className={`text-[11px] px-2 py-0.5 rounded border ${
                selectedLap === l.lap
                  ? 'bg-accent/20 border-accent text-accent'
                  : 'border-border text-muted hover:text-white'
              }`}
            >
              L{l.lap}
            </button>
          );
        })}
      </div>
      <div className="flex-1 min-h-0">
        {loading ? (
          <div className="h-full flex items-center justify-center text-xs text-muted">Loading…</div>
        ) : (
          <LapTraceOverlay data={data} activeChannels={activeChannels} />
        )}
      </div>
    </CardWrap>
  );
}

function BrakePointCompareCard({ uid }: { uid: string }) {
  const { data, loading, error } = useLapCompare(uid, null);

  return (
    <CardWrap title="Brake-point comparison vs best lap">
      {loading ? (
        <div className="h-full flex items-center justify-center text-xs text-muted">Loading…</div>
      ) : !data ? (
        <div className="h-full flex items-center justify-center text-xs text-muted">
          {error ?? 'Not enough hi-freq data to compare yet'}
        </div>
      ) : data.corners.length === 0 ? (
        <div className="text-xs text-muted">{data.note ?? 'No curated corners for this track id'}</div>
      ) : (
        <div className="overflow-y-auto h-full">
          <div className="text-xs text-muted mb-2">
            Lap {data.lap} ({formatLapMs(data.lap_time_ms)}) vs best lap {data.best_lap} (
            {formatLapMs(data.best_lap_time_ms)}) →{' '}
            <span className={deltaClass(data.delta_total_ms)}>{formatDeltaMs(data.delta_total_ms)}</span>
          </div>
          <CornerTable corners={data.corners} />
        </div>
      )}
    </CardWrap>
  );
}

function CornerTable({ corners }: { corners: CornerCompare[] }) {
  return (
    <table className="w-full text-xs">
      <thead className="text-[10px] text-muted uppercase tracking-wider">
        <tr>
          <th className="text-left font-normal pb-1">Corner</th>
          <th className="text-right font-normal pb-1">ΔBrake</th>
          <th className="text-right font-normal pb-1">ΔApex Speed</th>
          <th className="text-right font-normal pb-1">ΔExit Throttle</th>
        </tr>
      </thead>
      <tbody>
        {corners.map((c) => (
          <tr key={c.id} className="border-t border-border">
            <td className="py-1 text-text">
              <div className="font-bold">{c.id}</div>
              <div className="text-muted text-[10px]">{c.name}</div>
            </td>
            <td className={`py-1 text-right tabular-nums ${deltaClass(c.delta_brake_point_m)}`}>
              {c.delta_brake_point_m != null
                ? `${c.delta_brake_point_m > 0 ? '+' : ''}${c.delta_brake_point_m.toFixed(1)}m`
                : '—'}
            </td>
            <td className={`py-1 text-right tabular-nums ${deltaClass(c.delta_apex_speed_kmh, false)}`}>
              {c.delta_apex_speed_kmh != null
                ? `${c.delta_apex_speed_kmh > 0 ? '+' : ''}${c.delta_apex_speed_kmh.toFixed(1)} km/h`
                : '—'}
            </td>
            <td className={`py-1 text-right tabular-nums ${deltaClass(c.delta_exit_throttle, false)}`}>
              {c.delta_exit_throttle != null
                ? `${c.delta_exit_throttle > 0 ? '+' : ''}${(c.delta_exit_throttle * 100).toFixed(1)}%`
                : '—'}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function TyreWearCard({ summary }: { summary: SessionSummary }) {
  const data = summary.tyre_wear.map((s) => ({
    lap: s.lap,
    FL: s.wear_fl,
    FR: s.wear_fr,
    RL: s.wear_rl,
    RR: s.wear_rr,
  }));
  return (
    <CardWrap title="Tyre Wear (%)" className="h-[240px]">
      {data.length === 0 ? (
        <div className="h-full flex items-center justify-center text-xs text-muted">No wear data</div>
      ) : (
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={data} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
            <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
            <XAxis dataKey="lap" tick={{ fill: '#8b949e', fontSize: 11 }} stroke="#30363d" />
            <YAxis tick={{ fill: '#8b949e', fontSize: 11 }} stroke="#30363d" />
            <Tooltip
              contentStyle={{
                background: '#161b22',
                border: '1px solid #30363d',
                borderRadius: '6px',
                fontSize: '11px',
              }}
              labelStyle={{ color: '#c9d1d9' }}
            />
            <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
            <Line type="monotone" dataKey="FL" stroke="#58a6ff" dot={false} />
            <Line type="monotone" dataKey="FR" stroke="#bc8cff" dot={false} />
            <Line type="monotone" dataKey="RL" stroke="#FFD700" dot={false} />
            <Line type="monotone" dataKey="RR" stroke="#FF9300" dot={false} />
          </LineChart>
        </ResponsiveContainer>
      )}
    </CardWrap>
  );
}

function FuelCard({ summary }: { summary: SessionSummary }) {
  const data = summary.fuel.map((s) => ({
    lap: s.lap,
    fuel: s.fuel_in_tank,
    laps_left: s.fuel_laps_left,
  }));
  return (
    <CardWrap title="Fuel (kg)" className="h-[240px]">
      {data.length === 0 ? (
        <div className="h-full flex items-center justify-center text-xs text-muted">No fuel data</div>
      ) : (
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={data} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
            <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
            <XAxis dataKey="lap" tick={{ fill: '#8b949e', fontSize: 11 }} stroke="#30363d" />
            <YAxis
              tick={{ fill: '#8b949e', fontSize: 11 }}
              stroke="#30363d"
              tickFormatter={(v) => v.toFixed(0)}
            />
            <Tooltip
              contentStyle={{
                background: '#161b22',
                border: '1px solid #30363d',
                borderRadius: '6px',
                fontSize: '11px',
              }}
              labelStyle={{ color: '#c9d1d9' }}
            />
            <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
            <Line type="monotone" dataKey="fuel" stroke="#FF9300" name="Fuel kg" dot={false} />
            <Line type="monotone" dataKey="laps_left" stroke="#58a6ff" name="Laps left" dot={false} strokeDasharray="3 3" />
          </LineChart>
        </ResponsiveContainer>
      )}
    </CardWrap>
  );
}

function PositionCard({ summary }: { summary: SessionSummary }) {
  const data = summary.laps
    .filter((l) => l.position != null)
    .map((l) => ({ lap: l.lap, position: l.position }));
  if (data.length === 0) return null;
  const maxPos = Math.max(...data.map((d) => d.position!));
  return (
    <CardWrap title="Position over Race" className="h-[240px]">
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={data} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis dataKey="lap" tick={{ fill: '#8b949e', fontSize: 11 }} stroke="#30363d" />
          <YAxis
            tick={{ fill: '#8b949e', fontSize: 11 }}
            stroke="#30363d"
            reversed
            domain={[1, maxPos + 1]}
            allowDecimals={false}
          />
          <Tooltip
            contentStyle={{
              background: '#161b22',
              border: '1px solid #30363d',
              borderRadius: '6px',
              fontSize: '11px',
            }}
            labelStyle={{ color: '#c9d1d9' }}
            formatter={(v: number) => [`P${v}`, 'Position']}
          />
          <Line type="stepAfter" dataKey="position" stroke="#2ea043" strokeWidth={2} dot={false} />
        </LineChart>
      </ResponsiveContainer>
    </CardWrap>
  );
}

function IncidentLog({ events }: { events: EventSummary[] }) {
  if (events.length === 0) {
    return (
      <CardWrap title="Incidents & Events">
        <div className="text-xs text-muted text-center py-2">Nothing notable happened.</div>
      </CardWrap>
    );
  }
  return (
    <CardWrap title={`Events (${events.length})`}>
      <div className="overflow-y-auto h-full">
        <table className="w-full text-xs">
          <thead className="text-[10px] text-muted uppercase tracking-wider">
            <tr>
              <th className="text-left font-normal pb-1">When</th>
              <th className="text-left font-normal pb-1">Code</th>
              <th className="text-left font-normal pb-1">Detail</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e, i) => (
              <tr key={`${e.timestamp}-${i}`} className="border-t border-border">
                <td className="py-1 text-muted">{formatDate(e.timestamp)}</td>
                <td className="py-1 text-text">
                  <div className="font-mono text-[10px]">{e.code}</div>
                  <div className="text-[11px]">{e.label}</div>
                </td>
                <td className="py-1 text-muted">{e.detail || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </CardWrap>
  );
}

function SessionPicker({
  sessions,
  selected,
  onSelect,
}: {
  sessions: ReturnType<typeof useSessionsList>['sessions'];
  selected: string | null;
  onSelect: (uid: string) => void;
}) {
  if (!sessions || sessions.length === 0) return null;
  return (
    <select
      value={selected ?? ''}
      onChange={(e) => onSelect(e.target.value)}
      className="bg-bg text-white border border-border rounded-md px-3 py-1.5 text-sm focus:outline-none focus:border-accent"
    >
      <option value="" disabled>
        Pick a session…
      </option>
      {sessions.map((s) => (
        <option key={s.session_uid} value={s.session_uid}>
          {s.track_name} · {s.session_type_name} · {formatDate(s.last_seen)}
          {s.final_position ? ` · P${s.final_position}` : ''}
        </option>
      ))}
    </select>
  );
}

export default function DeepInsights() {
  const [params, setParams] = useSearchParams();
  const { sessions, loading: sessionsLoading } = useSessionsList();
  const [selected, setSelected] = useState<string | null>(params.get('uid'));

  // Default to most recent session if none chosen.
  useEffect(() => {
    if (selected) return;
    if (sessions && sessions.length > 0) {
      setSelected(sessions[0].session_uid);
    }
  }, [sessions, selected]);

  // Sync URL.
  useEffect(() => {
    if (selected && params.get('uid') !== selected) {
      const next = new URLSearchParams(params);
      next.set('uid', selected);
      setParams(next, { replace: true });
    }
  }, [selected, params, setParams]);

  const { summary, loading, error } = useSessionSummary(selected);
  const bestLap = useMemo(() => {
    if (!summary?.best_lap_ms) return null;
    return summary.laps.find((l) => l.lap_time_ms === summary.best_lap_ms)?.lap ?? null;
  }, [summary]);

  return (
    <div className="h-full overflow-y-auto p-6">
      <div className="flex items-start justify-between mb-4 flex-wrap gap-3">
        <div>
          <h1 className="text-2xl font-bold text-white">Deep Insights</h1>
          <p className="text-sm text-muted mt-1">
            Post-race analysis: lap traces, brake-point deltas, sector heatmaps,
            tire/fuel curves.
          </p>
        </div>
        <SessionPicker
          sessions={sessions}
          selected={selected}
          onSelect={setSelected}
        />
      </div>

      {sessionsLoading && (
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted">
          Loading sessions…
        </div>
      )}

      {!sessionsLoading && (!sessions || sessions.length === 0) && (
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted">
          No sessions captured yet. Drive a session in F1 25 to populate this view.
        </div>
      )}

      {selected && loading && (
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted">
          Loading session…
        </div>
      )}

      {selected && error && (
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-danger">
          Failed to load: {error}
        </div>
      )}

      {summary && (
        <>
          <div className="grid grid-cols-1 lg:grid-cols-[1.6fr_1fr] gap-3 mb-3">
            <div className="bg-panel border border-border rounded-lg p-3.5">
              <div className="text-xs text-muted uppercase tracking-wider mb-1">
                {summary.session_type_name} · {summary.track_name}
              </div>
              <div className="flex flex-wrap items-baseline gap-x-6 gap-y-1">
                <div>
                  <div className="text-[11px] text-muted">Laps</div>
                  <div className="text-lg text-white tabular-nums">{summary.laps.length}</div>
                </div>
                <div>
                  <div className="text-[11px] text-muted">Best Lap</div>
                  <div className="text-lg text-accent tabular-nums">
                    {formatLapMs(summary.best_lap_ms)}
                  </div>
                </div>
                {summary.final_position != null && (
                  <div>
                    <div className="text-[11px] text-muted">Final</div>
                    <div className="text-lg text-success tabular-nums">
                      P{summary.final_position}
                    </div>
                  </div>
                )}
                <div>
                  <div className="text-[11px] text-muted">Track Length</div>
                  <div className="text-lg text-white tabular-nums">
                    {(summary.track_length_m / 1000).toFixed(2)} km
                  </div>
                </div>
                <div>
                  <div className="text-[11px] text-muted">Started</div>
                  <div className="text-lg text-muted tabular-nums">
                    {formatDate(summary.started_at)}
                  </div>
                </div>
              </div>
            </div>
            <div className="bg-panel border border-border rounded-lg p-3.5 text-xs text-muted">
              <div className="text-[11px] uppercase tracking-wider mb-1">Session UID</div>
              <div className="font-mono text-text break-all">{summary.session_uid}</div>
              <div className="mt-2 text-[11px]">
                Player car index: <span className="text-text">{summary.player_car_index}</span>
              </div>
            </div>
          </div>

          <div className="grid grid-cols-1 lg:grid-cols-[1.5fr_1fr] gap-3 mb-3">
            <LapTimeChart summary={summary} />
            <SectorHeatmap laps={summary.laps} bestLapMs={summary.best_lap_ms} />
          </div>

          <div className="grid grid-cols-1 gap-3 mb-3">
            <LapTraceCard uid={summary.session_uid} laps={summary.laps} bestLap={bestLap} />
          </div>

          <div className="grid grid-cols-1 lg:grid-cols-2 gap-3 mb-3">
            <BrakePointCompareCard uid={summary.session_uid} />
            <PositionCard summary={summary} />
          </div>

          <div className="grid grid-cols-1 lg:grid-cols-2 gap-3 mb-3">
            <TyreWearCard summary={summary} />
            <FuelCard summary={summary} />
          </div>

          <div className="grid grid-cols-1 gap-3 mb-3">
            <IncidentLog events={summary.events} />
          </div>
        </>
      )}
    </div>
  );
}
