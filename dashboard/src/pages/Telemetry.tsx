import { useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import {
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { useTelemetryStream } from '../context/WebSocketContext';
import {
  useLapDelta,
  useLapList,
  useLapTraces,
  useSessionsList,
} from '../hooks/useSessions';
import { useChannelBuffer, type ChannelFrame } from '../hooks/useChannelBuffer';
import {
  ALL_CHANNELS,
  CardWrap,
  LAP_COLORS,
  deltaClass,
  formatDeltaMs,
  formatLapMs,
} from '../components/charts/shared';
import { LapTraceOverlay } from '../components/charts/LapTraceOverlay';
import type { LapListItem, SessionListItem } from '../types/sessions';

// Telemetry.tsx is the live + comparison surface for the channels every
// driver and the race-engineer AI both reason about.
//
//   Live tab: rolling time-window charts (throttle/brake, speed/gear,
//             steering, G-forces, brake temps, tyre temps, ERS+fuel)
//             fed by the existing WS stream — zero backend roundtrips.
//   Compare tab: multi-lap overlay (best + up to 3 picked laps, distinct
//             colours) plus a cumulative ms delta strip vs the reference
//             lap. Same /api/laps/{traces,delta,list} the analyst + Live
//             agent tools call, so what the driver sees matches what the
//             AI cites on the radio.

type Tab = 'live' | 'compare';

const WINDOW_OPTIONS: { s: number; label: string }[] = [
  { s: 5, label: '5s' },
  { s: 15, label: '15s' },
  { s: 30, label: '30s' },
  { s: 60, label: '60s' },
];

const MAX_LAPS_OVERLAY = 4; // best auto-pinned + 3 driver-picked

export default function Telemetry() {
  const [params, setParams] = useSearchParams();
  const initialTab: Tab = params.get('tab') === 'compare' ? 'compare' : 'live';
  const [tab, setTab] = useState<Tab>(initialTab);

  useEffect(() => {
    const next = new URLSearchParams(params);
    if (tab === 'live') next.delete('tab');
    else next.set('tab', tab);
    setParams(next, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab]);

  return (
    <div className="h-full overflow-y-auto p-6">
      <div className="flex items-start justify-between mb-4 flex-wrap gap-3">
        <div>
          <h1 className="text-2xl font-bold text-white">Telemetry</h1>
          <p className="text-sm text-muted mt-1">
            Live channel charts and multi-lap overlay comparison — the same
            data the analyst and Gemini Live tools cite over the radio.
          </p>
        </div>
        <TabSwitch active={tab} onSelect={setTab} />
      </div>

      {tab === 'live' ? <LiveTab /> : <CompareTab />}
    </div>
  );
}

function TabSwitch({ active, onSelect }: { active: Tab; onSelect: (t: Tab) => void }) {
  return (
    <div className="inline-flex bg-panel border border-border rounded-md overflow-hidden">
      {(['live', 'compare'] as Tab[]).map((t) => (
        <button
          key={t}
          onClick={() => onSelect(t)}
          className={`px-3 py-1.5 text-sm capitalize transition-colors ${
            active === t
              ? 'bg-accent/20 text-accent'
              : 'text-muted hover:text-white'
          }`}
        >
          {t === 'live' ? 'Live' : 'Compare Laps'}
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Live tab — rolling buffer of WS frames, broken into 7 channel cards.
// ---------------------------------------------------------------------------

function LiveTab() {
  const [windowSec, setWindowSec] = useState<number>(30);
  const frames = useChannelBuffer({ windowSec });
  const { connected } = useTelemetryStream();

  return (
    <>
      <div className="flex items-center gap-2 mb-3 flex-wrap">
        <span className="text-xs text-muted uppercase tracking-wider">Window:</span>
        {WINDOW_OPTIONS.map((opt) => (
          <button
            key={opt.s}
            onClick={() => setWindowSec(opt.s)}
            className={`text-[11px] px-2 py-0.5 rounded border ${
              windowSec === opt.s
                ? 'bg-accent/20 border-accent text-accent'
                : 'border-border text-muted hover:text-white'
            }`}
          >
            {opt.label}
          </button>
        ))}
        <span className="ml-auto text-[11px] text-muted">
          {connected ? `${frames.length} samples` : 'Telemetry stream offline — waiting…'}
        </span>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
        <InputsCard frames={frames} />
        <SpeedGearCard frames={frames} />
        <SteeringCard frames={frames} />
        <GForceCard frames={frames} />
        <BrakeTempCard frames={frames} />
        <TyreTempCard frames={frames} />
        <EnergyCard frames={frames} />
        <RPMCard frames={frames} />
      </div>
    </>
  );
}

function currentValue<K extends keyof ChannelFrame>(
  frames: ChannelFrame[],
  key: K,
): ChannelFrame[K] | null {
  if (frames.length === 0) return null;
  return frames[frames.length - 1][key];
}

function LiveCard({
  title,
  current,
  height = 200,
  children,
}: {
  title: string;
  current?: string | number | null;
  height?: number;
  children: React.ReactNode;
}) {
  return (
    <CardWrap
      title={title}
      className=""
      right={
        current != null ? (
          <span className="text-sm text-white tabular-nums font-semibold">
            {current}
          </span>
        ) : null
      }
    >
      <div style={{ height }}>{children}</div>
    </CardWrap>
  );
}

// commonAxisProps returns the recharts axis props every live card uses, so
// stylistic drift between cards stays minimal as we iterate.
function commonAxes(unit?: string) {
  return {
    xAxis: (
      <XAxis
        dataKey="t"
        type="number"
        domain={['dataMin', 'dataMax']}
        tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
        tick={{ fill: '#8b949e', fontSize: 10 }}
        stroke="#30363d"
      />
    ),
    yAxis: (
      <YAxis
        tick={{ fill: '#8b949e', fontSize: 10 }}
        stroke="#30363d"
        tickFormatter={unit ? (v: number) => `${v.toFixed(0)}${unit}` : undefined}
      />
    ),
  };
}

function liveTooltip() {
  return (
    <Tooltip
      contentStyle={{
        background: '#161b22',
        border: '1px solid #30363d',
        borderRadius: '6px',
        fontSize: '11px',
      }}
      labelStyle={{ color: '#c9d1d9' }}
      labelFormatter={(v: number) => `${(v / 1000).toFixed(1)}s`}
    />
  );
}

function InputsCard({ frames }: { frames: ChannelFrame[] }) {
  const throttle = currentValue(frames, 'throttle');
  const brake = currentValue(frames, 'brake');
  const cur =
    throttle != null && brake != null
      ? `T ${Math.round((throttle ?? 0) * 100)}% / B ${Math.round((brake ?? 0) * 100)}%`
      : '—';
  const { xAxis, yAxis } = commonAxes();
  return (
    <LiveCard title="Throttle & Brake" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          {xAxis}
          <YAxis
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            domain={[0, 1]}
            tickFormatter={(v) => `${Math.round(v * 100)}%`}
          />
          {liveTooltip()}
          <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
          <Line
            type="monotone"
            dataKey="throttle"
            name="Throttle"
            stroke="#2ea043"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
          <Line
            type="monotone"
            dataKey="brake"
            name="Brake"
            stroke="#f85149"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
          {/* unused yAxis hint keeps the shared helper return ergonomic */}
          <></>
          {yAxis ? null : null}
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function SpeedGearCard({ frames }: { frames: ChannelFrame[] }) {
  const speed = currentValue(frames, 'speed');
  const gear = currentValue(frames, 'gear');
  const cur = speed != null ? `${Math.round(speed ?? 0)} km/h · G${gear ?? '—'}` : '—';
  return (
    <LiveCard title="Speed & Gear" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 28, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis
            yAxisId="speed"
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            domain={[0, 360]}
            tickFormatter={(v) => `${v}`}
          />
          <YAxis
            yAxisId="gear"
            orientation="right"
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            domain={[-1, 8]}
            ticks={[0, 2, 4, 6, 8]}
          />
          {liveTooltip()}
          <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
          <Line
            yAxisId="speed"
            type="monotone"
            dataKey="speed"
            name="Speed (km/h)"
            stroke="#58a6ff"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
          <Line
            yAxisId="gear"
            type="stepAfter"
            dataKey="gear"
            name="Gear"
            stroke="#bc8cff"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function SteeringCard({ frames }: { frames: ChannelFrame[] }) {
  const steering = currentValue(frames, 'steering');
  const cur = steering != null ? `${((steering ?? 0) * 100).toFixed(0)}%` : '—';
  return (
    <LiveCard title="Steering" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            domain={[-1, 1]}
            tickFormatter={(v) => `${(v * 100).toFixed(0)}%`}
          />
          <ReferenceLine y={0} stroke="#30363d" strokeDasharray="2 2" />
          {liveTooltip()}
          <Line
            type="monotone"
            dataKey="steering"
            name="Steering"
            stroke="#FFD700"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function GForceCard({ frames }: { frames: ChannelFrame[] }) {
  const lat = currentValue(frames, 'g_lat');
  const lon = currentValue(frames, 'g_lon');
  const cur =
    lat != null
      ? `Lat ${(lat ?? 0).toFixed(2)}g · Lon ${(lon ?? 0).toFixed(2)}g`
      : '—';
  return (
    <LiveCard title="G-Forces (lat / long.)" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            domain={[-5, 5]}
            tickFormatter={(v) => `${v.toFixed(1)}g`}
          />
          <ReferenceLine y={0} stroke="#30363d" strokeDasharray="2 2" />
          {liveTooltip()}
          <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
          <Line
            type="monotone"
            dataKey="g_lat"
            name="Lateral"
            stroke="#bc8cff"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
          <Line
            type="monotone"
            dataKey="g_lon"
            name="Long."
            stroke="#FF9300"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function BrakeTempCard({ frames }: { frames: ChannelFrame[] }) {
  const cur =
    frames.length > 0
      ? `${Math.round(frames[frames.length - 1].brake_temp_avg)}°C avg`
      : '—';
  return (
    <LiveCard title="Brake Temps (per wheel)" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis tick={{ fill: '#8b949e', fontSize: 10 }} stroke="#30363d" />
          {liveTooltip()}
          <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
          <Line type="monotone" dataKey="brake_temp_fl" name="FL" stroke="#58a6ff" strokeWidth={1} dot={false} isAnimationActive={false} />
          <Line type="monotone" dataKey="brake_temp_fr" name="FR" stroke="#bc8cff" strokeWidth={1} dot={false} isAnimationActive={false} />
          <Line type="monotone" dataKey="brake_temp_rl" name="RL" stroke="#2ea043" strokeWidth={1} dot={false} isAnimationActive={false} />
          <Line type="monotone" dataKey="brake_temp_rr" name="RR" stroke="#FF9300" strokeWidth={1} dot={false} isAnimationActive={false} />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function TyreTempCard({ frames }: { frames: ChannelFrame[] }) {
  const cur =
    frames.length > 0
      ? `${Math.round(frames[frames.length - 1].tyre_surf_temp_avg)}°C avg`
      : '—';
  return (
    <LiveCard title="Tyre Surface Temps" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis tick={{ fill: '#8b949e', fontSize: 10 }} stroke="#30363d" />
          {liveTooltip()}
          <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
          <Line type="monotone" dataKey="tyre_temp_fl" name="FL" stroke="#58a6ff" strokeWidth={1} dot={false} isAnimationActive={false} />
          <Line type="monotone" dataKey="tyre_temp_fr" name="FR" stroke="#bc8cff" strokeWidth={1} dot={false} isAnimationActive={false} />
          <Line type="monotone" dataKey="tyre_temp_rl" name="RL" stroke="#2ea043" strokeWidth={1} dot={false} isAnimationActive={false} />
          <Line type="monotone" dataKey="tyre_temp_rr" name="RR" stroke="#FF9300" strokeWidth={1} dot={false} isAnimationActive={false} />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function EnergyCard({ frames }: { frames: ChannelFrame[] }) {
  const fuel = currentValue(frames, 'fuel');
  const ers = currentValue(frames, 'ers_store_pct');
  const cur =
    fuel != null && ers != null
      ? `Fuel ${(fuel ?? 0).toFixed(1)}kg · ERS ${Math.round(ers ?? 0)}%`
      : '—';
  return (
    <LiveCard title="Fuel & ERS" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 28, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis
            yAxisId="fuel"
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            tickFormatter={(v) => `${v.toFixed(0)}kg`}
          />
          <YAxis
            yAxisId="ers"
            orientation="right"
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
            domain={[0, 100]}
            tickFormatter={(v) => `${v}%`}
          />
          {liveTooltip()}
          <Legend wrapperStyle={{ fontSize: '11px', color: '#8b949e' }} />
          <Line yAxisId="fuel" type="monotone" dataKey="fuel" name="Fuel kg" stroke="#FF9300" strokeWidth={1.5} dot={false} isAnimationActive={false} />
          <Line yAxisId="ers" type="monotone" dataKey="ers_store_pct" name="ERS %" stroke="#FFD700" strokeWidth={1.5} dot={false} isAnimationActive={false} />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

function RPMCard({ frames }: { frames: ChannelFrame[] }) {
  const rpm = currentValue(frames, 'rpm');
  const cur = rpm != null ? `${Math.round(rpm ?? 0)} rpm` : '—';
  return (
    <LiveCard title="Engine RPM" current={cur}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={frames} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
          <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}s`}
            tick={{ fill: '#8b949e', fontSize: 10 }}
            stroke="#30363d"
          />
          <YAxis tick={{ fill: '#8b949e', fontSize: 10 }} stroke="#30363d" />
          {liveTooltip()}
          <Line type="monotone" dataKey="rpm" name="RPM" stroke="#f85149" strokeWidth={1.5} dot={false} isAnimationActive={false} />
        </LineChart>
      </ResponsiveContainer>
    </LiveCard>
  );
}

// ---------------------------------------------------------------------------
// Compare tab — multi-lap overlay + cumulative ms delta strip.
// ---------------------------------------------------------------------------

// PickedLap pins a single lap to its source session. The Compare tab keys
// every selection on this pair so the same lap-number from two different
// sessions never collides in the overlay or the delta strip.
interface PickedLap {
  uid: string;
  lap: number;
}

const EXTRA_SESSION_SLOTS = 3; // how many past sessions can be stacked

function CompareTab() {
  const { state } = useTelemetryStream();
  const uid =
    state?.session_uid && state.session_uid !== '0' ? state.session_uid : null;
  const trackId = state?.track_id ?? null;
  const { data: lapList, loading: listLoading } = useLapList(uid);
  const bestLap = lapList?.best_lap ?? null;

  // selectedLaps holds driver-picked laps (excluding the auto-pinned best).
  // Capped at MAX_LAPS_OVERLAY - 1 so total stays at 4 incl. best.
  const [selectedLaps, setSelectedLaps] = useState<PickedLap[]>([]);
  const [activeChannels, setActiveChannels] = useState<string[]>([
    'throttle',
    'brake',
    'speed',
  ]);

  // Extra sessions whose lap rosters are shown alongside the current one.
  // Capped at EXTRA_SESSION_SLOTS so we can keep the hook count static
  // (useLapList must be called the same number of times each render).
  const [extraSessionUids, setExtraSessionUids] = useState<string[]>([]);
  const [showAllTracks, setShowAllTracks] = useState(false);

  // Fixed-slot lap-list fetches for the extra sessions. Past sessions don't
  // change so we pass refreshMs=0 to avoid useless polling.
  const extra0 = useLapList(extraSessionUids[0] ?? null, 0);
  const extra1 = useLapList(extraSessionUids[1] ?? null, 0);
  const extra2 = useLapList(extraSessionUids[2] ?? null, 0);
  const extraLapLists = [extra0, extra1, extra2];

  const { sessions, loading: sessionsLoading } = useSessionsList();

  // Available past sessions for the picker dropdown: anything that isn't
  // the current uid or already-stacked, filtered to current track unless
  // the user opts in.
  const availableSessions = useMemo(() => {
    if (!sessions) return [];
    return sessions.filter((s) => {
      if (uid && s.session_uid === uid) return false;
      if (extraSessionUids.includes(s.session_uid)) return false;
      if (!showAllTracks && trackId != null && s.track_id !== trackId) return false;
      return (s.total_laps ?? 0) > 0;
    });
  }, [sessions, uid, extraSessionUids, showAllTracks, trackId]);

  const sessionsByUid = useMemo(() => {
    const m = new Map<string, SessionListItem>();
    for (const s of sessions ?? []) m.set(s.session_uid, s);
    return m;
  }, [sessions]);

  // When lap rosters refresh, drop any selection whose lap no longer exists.
  useEffect(() => {
    const valid = new Map<string, Set<number>>();
    if (lapList && uid) valid.set(uid, new Set(lapList.laps.map((l) => l.lap)));
    for (let i = 0; i < EXTRA_SESSION_SLOTS; i++) {
      const u = extraSessionUids[i];
      const data = extraLapLists[i].data;
      if (u && data) valid.set(u, new Set(data.laps.map((l) => l.lap)));
    }
    setSelectedLaps((prev) =>
      prev.filter((p) => {
        if (p.uid === uid && p.lap === bestLap) return false;
        return valid.get(p.uid)?.has(p.lap) ?? false;
      }),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lapList, bestLap, uid, extra0.data, extra1.data, extra2.data]);

  // Build the laps= CSV. Current-session laps stay bare; cross-session laps
  // get the `N@<uid>` suffix the backend resolveLaps understands.
  const lapsCsv = useMemo(() => {
    const parts: string[] = [];
    if (bestLap) parts.push(String(bestLap));
    for (const p of selectedLaps) {
      parts.push(p.uid === uid ? String(p.lap) : `${p.lap}@${p.uid}`);
    }
    return parts.length > 0 ? parts.join(',') : 'last';
  }, [bestLap, selectedLaps, uid]);

  const tracesQuery = useMemo(
    () => ({ laps: lapsCsv, channels: activeChannels.join(','), buckets: 80 }),
    [lapsCsv, activeChannels],
  );
  const { data: traces, loading: tracesLoading } = useLapTraces(uid, tracesQuery);

  function toggleChannel(id: string) {
    setActiveChannels((prev) =>
      prev.includes(id) ? prev.filter((p) => p !== id) : [...prev, id],
    );
  }

  function toggleLap(lap: PickedLap) {
    if (lap.uid === uid && lap.lap === bestLap) return;
    setSelectedLaps((prev) => {
      const idx = prev.findIndex((p) => p.uid === lap.uid && p.lap === lap.lap);
      if (idx >= 0) return prev.filter((_, i) => i !== idx);
      if (prev.length >= MAX_LAPS_OVERLAY - 1) return prev;
      return [...prev, lap];
    });
  }

  function addSession(s: SessionListItem) {
    setExtraSessionUids((prev) => {
      if (prev.includes(s.session_uid)) return prev;
      if (prev.length >= EXTRA_SESSION_SLOTS) return prev;
      return [...prev, s.session_uid];
    });
  }

  function removeSession(sessionUid: string) {
    setExtraSessionUids((prev) => prev.filter((u) => u !== sessionUid));
    setSelectedLaps((prev) => prev.filter((p) => p.uid !== sessionUid));
  }

  if (!uid) {
    return (
      <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted">
        Waiting for a live session — start a race in F1 25 (or switch to mock
        mode in Settings).
      </div>
    );
  }

  const remaining = MAX_LAPS_OVERLAY - 1 - selectedLaps.length;
  const noLapsYet =
    (!lapList || lapList.laps.length === 0) && extraSessionUids.length === 0;

  return (
    <>
      <SessionPicker
        currentUid={uid}
        currentTrackName={sessionsByUid.get(uid)?.track_name ?? null}
        extras={extraSessionUids
          .map((u) => sessionsByUid.get(u))
          .filter((s): s is SessionListItem => Boolean(s))}
        available={availableSessions}
        showAllTracks={showAllTracks}
        onToggleAllTracks={() => setShowAllTracks((v) => !v)}
        onAddSession={addSession}
        onRemoveSession={removeSession}
        loading={sessionsLoading}
        canAddMore={extraSessionUids.length < EXTRA_SESSION_SLOTS}
      />

      {listLoading && !lapList ? (
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted mb-3">
          Loading lap roster…
        </div>
      ) : noLapsYet ? (
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted mb-3">
          No completed laps yet in this session. Finish at least one full lap,
          or add a past session above to compare against history.
        </div>
      ) : (
        <LapPicker
          currentUid={uid}
          currentLaps={lapList?.laps ?? []}
          currentBestLap={bestLap}
          extraSessions={extraSessionUids.map((u, i) => ({
            uid: u,
            meta: sessionsByUid.get(u) ?? null,
            laps: extraLapLists[i].data?.laps ?? [],
          }))}
          selected={selectedLaps}
          onToggle={toggleLap}
          remaining={remaining}
        />
      )}

      <ChannelPicker active={activeChannels} onToggle={toggleChannel} />

      {/* Cumulative ms delta strip. Only meaningful when at least one
          non-best lap is selected — best vs best is always zero. */}
      {selectedLaps.length > 0 && bestLap && (
        <DeltaStrip
          uid={uid}
          laps={selectedLaps}
          referenceLap={bestLap}
        />
      )}

      <CardWrap title="Lap Trace Overlay" className="h-[460px]">
        {tracesLoading ? (
          <div className="h-full flex items-center justify-center text-xs text-muted">
            Loading traces…
          </div>
        ) : (
          <LapTraceOverlay data={traces} activeChannels={activeChannels} />
        )}
      </CardWrap>
    </>
  );
}

function SessionPicker({
  currentUid,
  currentTrackName,
  extras,
  available,
  showAllTracks,
  onToggleAllTracks,
  onAddSession,
  onRemoveSession,
  loading,
  canAddMore,
}: {
  currentUid: string;
  currentTrackName: string | null;
  extras: SessionListItem[];
  available: SessionListItem[];
  showAllTracks: boolean;
  onToggleAllTracks: () => void;
  onAddSession: (s: SessionListItem) => void;
  onRemoveSession: (uid: string) => void;
  loading: boolean;
  canAddMore: boolean;
}) {
  function shortUid(u: string): string {
    return u.length > 6 ? `S${u.slice(-6)}` : `S${u}`;
  }
  function shortDate(iso: string): string {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    return d.toISOString().slice(0, 10);
  }
  return (
    <CardWrap title="Sessions" className="mb-3">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-[11px] px-2 py-0.5 rounded border bg-accent/20 border-accent text-accent">
          ● Live · {currentTrackName ?? 'current'} ({shortUid(currentUid)})
        </span>

        {extras.map((s) => (
          <span
            key={s.session_uid}
            className="text-[11px] px-2 py-0.5 rounded border border-border text-muted flex items-center gap-1"
            title={`${s.track_name} · ${s.session_type_name} · ${shortDate(s.last_seen)}`}
          >
            {s.track_name} · {s.session_type_name} · {shortDate(s.last_seen)}
            <button
              onClick={() => onRemoveSession(s.session_uid)}
              className="ml-1 text-danger hover:text-white"
              title="Remove session"
            >
              ×
            </button>
          </span>
        ))}

        <div className="ml-auto flex items-center gap-2">
          <label className="text-[11px] text-muted flex items-center gap-1 cursor-pointer">
            <input
              type="checkbox"
              checked={showAllTracks}
              onChange={onToggleAllTracks}
              className="accent-accent"
            />
            All tracks
          </label>
          <select
            value=""
            disabled={!canAddMore || loading || available.length === 0}
            onChange={(e) => {
              const picked = available.find((s) => s.session_uid === e.target.value);
              if (picked) onAddSession(picked);
              e.target.value = '';
            }}
            className="bg-panel border border-border rounded text-[11px] text-white px-2 py-0.5 max-w-[300px]"
          >
            <option value="" disabled>
              {loading
                ? 'Loading sessions…'
                : !canAddMore
                ? `Max ${EXTRA_SESSION_SLOTS} stacked`
                : available.length === 0
                ? showAllTracks
                  ? 'No other sessions yet'
                  : 'No past sessions for this track'
                : '+ Add session'}
            </option>
            {available.map((s) => (
              <option key={s.session_uid} value={s.session_uid}>
                {s.track_name} · {s.session_type_name} · {shortDate(s.last_seen)}
                {s.best_lap_ms ? ` · best ${formatLapMs(s.best_lap_ms)}` : ''}
              </option>
            ))}
          </select>
        </div>
      </div>
    </CardWrap>
  );
}

interface ExtraSessionGroup {
  uid: string;
  meta: SessionListItem | null;
  laps: LapListItem[];
}

function LapPicker({
  currentUid,
  currentLaps,
  currentBestLap,
  extraSessions,
  selected,
  onToggle,
  remaining,
}: {
  currentUid: string;
  currentLaps: LapListItem[];
  currentBestLap: number | null;
  extraSessions: ExtraSessionGroup[];
  selected: PickedLap[];
  onToggle: (lap: PickedLap) => void;
  remaining: number;
}) {
  function isSelected(uid: string, lap: number): boolean {
    return selected.some((p) => p.uid === uid && p.lap === lap);
  }
  function bestFromLaps(laps: LapListItem[]): number | null {
    let bestLap: number | null = null;
    let bestMs = Number.POSITIVE_INFINITY;
    for (const l of laps) {
      if (!l.valid || l.lap_time_ms <= 0) continue;
      if (l.lap_time_ms < bestMs) {
        bestMs = l.lap_time_ms;
        bestLap = l.lap;
      }
    }
    return bestLap;
  }

  return (
    <CardWrap
      title={`Lap Picker (best pinned · ${remaining} more allowed)`}
      className="mb-3"
    >
      <div className="flex flex-col gap-2">
        {/* Current session row — best is auto-pinned. */}
        <SessionLapRow
          label="Live"
          labelClassName="bg-accent/20 border-accent text-accent"
          uid={currentUid}
          laps={currentLaps}
          bestLap={currentBestLap}
          pinBest
          isSelected={isSelected}
          onToggle={onToggle}
          remaining={remaining}
        />
        {extraSessions.map((group) => {
          const trackName = group.meta?.track_name ?? '—';
          const dateLabel = group.meta?.last_seen
            ? new Date(group.meta.last_seen).toISOString().slice(0, 10)
            : '';
          const groupBest = bestFromLaps(group.laps);
          return (
            <SessionLapRow
              key={group.uid}
              label={`${trackName} · ${dateLabel}`}
              labelClassName="border-border text-muted"
              uid={group.uid}
              laps={group.laps}
              bestLap={groupBest}
              pinBest={false}
              isSelected={isSelected}
              onToggle={onToggle}
              remaining={remaining}
            />
          );
        })}
      </div>
    </CardWrap>
  );
}

function SessionLapRow({
  label,
  labelClassName,
  uid,
  laps,
  bestLap,
  pinBest,
  isSelected,
  onToggle,
  remaining,
}: {
  label: string;
  labelClassName: string;
  uid: string;
  laps: LapListItem[];
  bestLap: number | null;
  pinBest: boolean;
  isSelected: (uid: string, lap: number) => boolean;
  onToggle: (lap: PickedLap) => void;
  remaining: number;
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className={`text-[11px] px-2 py-0.5 rounded border ${labelClassName}`}>
        {label}
      </span>
      {laps.length === 0 ? (
        <span className="text-[11px] text-muted/60 italic">
          no completed laps in this session
        </span>
      ) : null}
      {pinBest && bestLap ? (
        <span className="text-[11px] px-2 py-0.5 rounded border bg-warning/20 border-warning text-warning">
          ★ Best · L{bestLap}
        </span>
      ) : null}
      {laps.map((l) => {
        if (pinBest && l.lap === bestLap) return null;
        const active = isSelected(uid, l.lap);
        const disabled = !active && remaining <= 0;
        const isBestOfGroup = !pinBest && l.lap === bestLap;
        return (
          <button
            key={`${uid}-${l.lap}`}
            onClick={() => onToggle({ uid, lap: l.lap })}
            disabled={disabled}
            className={`text-[11px] px-2 py-0.5 rounded border transition-colors ${
              active
                ? 'bg-accent/20 border-accent text-accent'
                : disabled
                ? 'border-border text-muted/40 cursor-not-allowed'
                : isBestOfGroup
                ? 'border-warning text-warning hover:bg-warning/20'
                : 'border-border text-muted hover:text-white'
            }`}
            title={`Lap ${l.lap} — ${formatLapMs(l.lap_time_ms)}${l.valid ? '' : ' (invalid)'}`}
          >
            {isBestOfGroup ? '★ ' : ''}L{l.lap} · {formatLapMs(l.lap_time_ms)}
            {!l.valid && <span className="ml-1 text-danger">●</span>}
          </button>
        );
      })}
    </div>
  );
}

function ChannelPicker({
  active,
  onToggle,
}: {
  active: string[];
  onToggle: (id: string) => void;
}) {
  return (
    <div className="flex flex-wrap gap-1.5 mb-3">
      {ALL_CHANNELS.map((ch) => {
        const isActive = active.includes(ch.id);
        return (
          <button
            key={ch.id}
            onClick={() => onToggle(ch.id)}
            className={`text-[11px] px-2 py-0.5 rounded border transition-colors ${
              isActive
                ? 'bg-accent/20 border-accent text-accent'
                : 'border-border text-muted hover:text-white'
            }`}
          >
            {ch.label}
          </button>
        );
      })}
    </div>
  );
}

function DeltaStrip({
  uid,
  laps,
  referenceLap,
}: {
  uid: string;
  laps: PickedLap[];
  referenceLap: number;
}) {
  // Fixed-slot useLapDelta calls — hook rules forbid calling in a loop, so
  // we cap selection at MAX_LAPS_OVERLAY-1 and pass null for empty slots.
  // Each slot can target a different session via lap_session_uid; the
  // backend rejects mismatched track lengths so the math stays correct.
  function deltaQueryFor(slot: PickedLap | null) {
    return {
      lap: slot ? String(slot.lap) : null,
      reference: String(referenceLap),
      buckets: 80,
      lapSessionUid: slot && slot.uid !== uid ? slot.uid : null,
    };
  }

  const slot1 = laps[0] ?? null;
  const slot2 = laps[1] ?? null;
  const slot3 = laps[2] ?? null;

  const d1 = useLapDelta(slot1 ? uid : null, deltaQueryFor(slot1));
  const d2 = useLapDelta(slot2 ? uid : null, deltaQueryFor(slot2));
  const d3 = useLapDelta(slot3 ? uid : null, deltaQueryFor(slot3));

  function slotKey(slot: PickedLap | null): string {
    if (!slot) return '';
    return slot.uid === uid ? `L${slot.lap}` : `L${slot.lap}@${slot.uid.slice(-4)}`;
  }

  const key1 = slotKey(slot1);
  const key2 = slotKey(slot2);
  const key3 = slotKey(slot3);

  const chartData = useMemo(() => {
    const datasets = [d1.data, d2.data, d3.data].filter(Boolean);
    if (datasets.length === 0) return [];
    // All deltas share the same distance bucketing — backend enforces a
    // matching track_length when cross-session.
    const ref = datasets[0]!;
    return ref.distance_m.map((dist, i) => {
      const row: Record<string, number | null> = { distance: dist };
      if (d1.data && key1) row[key1] = d1.data.delta_ms[i] ?? null;
      if (d2.data && key2) row[key2] = d2.data.delta_ms[i] ?? null;
      if (d3.data && key3) row[key3] = d3.data.delta_ms[i] ?? null;
      return row;
    });
  }, [d1.data, d2.data, d3.data, key1, key2, key3]);

  const anyLoading = d1.loading || d2.loading || d3.loading;
  const anyError = d1.error || d2.error || d3.error;

  // Final delta = last non-null value per lap. Used in the legend so the
  // total lap delta is visible at a glance.
  function tailDelta(deltas: (number | null)[] | undefined): number | null {
    if (!deltas) return null;
    for (let i = deltas.length - 1; i >= 0; i--) {
      if (deltas[i] != null) return deltas[i] as number;
    }
    return null;
  }
  const finals: (number | null)[] = [
    tailDelta(d1.data?.delta_ms),
    tailDelta(d2.data?.delta_ms),
    tailDelta(d3.data?.delta_ms),
  ];

  return (
    <CardWrap title={`Cumulative Delta vs L${referenceLap} (best of live session)`} className="h-[200px] mb-3">
      {anyError && chartData.length === 0 ? (
        <div className="h-full flex items-center justify-center text-xs text-danger px-4 text-center">
          {anyError}
        </div>
      ) : anyLoading && chartData.length === 0 ? (
        <div className="h-full flex items-center justify-center text-xs text-muted">
          Loading delta…
        </div>
      ) : chartData.length === 0 ? (
        <div className="h-full flex items-center justify-center text-xs text-muted">
          No delta data — pick at least one lap.
        </div>
      ) : (
        <>
          <div className="flex flex-wrap gap-3 mb-1 text-[11px] text-muted">
            {laps.map((p, idx) => {
              const final = finals[idx];
              const label = p.uid === uid ? `L${p.lap}` : `L${p.lap} · S${p.uid.slice(-4)}`;
              return (
                <span key={`${p.uid}-${p.lap}`}>
                  <span
                    className="inline-block w-2 h-2 rounded-full mr-1 align-middle"
                    style={{ backgroundColor: LAP_COLORS[(idx + 1) % LAP_COLORS.length] }}
                  />
                  {label}: <span className={deltaClass(final == null ? undefined : final)}>{formatDeltaMs(final)}</span>
                </span>
              );
            })}
          </div>
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={chartData} margin={{ top: 5, right: 12, left: 0, bottom: 5 }}>
              <CartesianGrid stroke="#30363d" strokeDasharray="3 3" />
              <XAxis
                dataKey="distance"
                tick={{ fill: '#8b949e', fontSize: 10 }}
                stroke="#30363d"
                tickFormatter={(v) => `${(v / 1000).toFixed(1)}km`}
              />
              <YAxis
                tick={{ fill: '#8b949e', fontSize: 10 }}
                stroke="#30363d"
                tickFormatter={(v) => `${(v / 1000).toFixed(2)}s`}
              />
              <ReferenceLine y={0} stroke="#30363d" strokeDasharray="2 2" />
              <Tooltip
                contentStyle={{
                  background: '#161b22',
                  border: '1px solid #30363d',
                  borderRadius: '6px',
                  fontSize: '11px',
                }}
                labelStyle={{ color: '#c9d1d9' }}
                labelFormatter={(v: number) => `${(v / 1000).toFixed(2)} km`}
                formatter={(v: number) => `${(v / 1000).toFixed(3)}s`}
              />
              {slot1 && (
                <Line
                  type="monotone"
                  dataKey={key1}
                  stroke={LAP_COLORS[1]}
                  strokeWidth={1.5}
                  dot={false}
                  isAnimationActive={false}
                />
              )}
              {slot2 && (
                <Line
                  type="monotone"
                  dataKey={key2}
                  stroke={LAP_COLORS[2]}
                  strokeWidth={1.5}
                  dot={false}
                  isAnimationActive={false}
                />
              )}
              {slot3 && (
                <Line
                  type="monotone"
                  dataKey={key3}
                  stroke={LAP_COLORS[3]}
                  strokeWidth={1.5}
                  dot={false}
                  isAnimationActive={false}
                />
              )}
            </LineChart>
          </ResponsiveContainer>
        </>
      )}
    </CardWrap>
  );
}
