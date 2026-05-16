import { Link } from 'react-router-dom';
import {
  PieChart,
  Pie,
  Cell,
  Tooltip,
  ResponsiveContainer,
  Legend,
} from 'recharts';
import { useCareerStats } from '../hooks/useCareerStats';
import type { CareerStats, RecentSessionStat, TrackStat } from '../types/career';
import { COMPOUND_COLORS } from '../lib/constants';

const COMPOUND_LABEL: Record<string, string> = {
  soft: 'Soft',
  medium: 'Medium',
  hard: 'Hard',
  inter: 'Inter',
  wet: 'Wet',
};

const COMPOUND_HEX: Record<string, string> = {
  soft: COMPOUND_COLORS[16],
  medium: COMPOUND_COLORS[17],
  hard: COMPOUND_COLORS[18],
  inter: COMPOUND_COLORS[7],
  wet: COMPOUND_COLORS[8],
};

function formatLapMs(ms: number | null): string {
  if (!ms || ms <= 0) return '—';
  const totalSec = ms / 1000;
  const m = Math.floor(totalSec / 60);
  const s = totalSec - m * 60;
  return `${m}:${s.toFixed(3).padStart(6, '0')}`;
}

function formatHours(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (h === 0) return `${m}m`;
  return `${h}h ${m}m`;
}

function formatDate(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
  });
}

function StatCard({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="bg-panel border border-border rounded-lg p-4">
      <div className="text-[11px] text-muted uppercase tracking-wider">{label}</div>
      <div className="text-2xl font-bold text-white tabular-nums mt-1">{value}</div>
      {sub && <div className="text-xs text-muted mt-1">{sub}</div>}
    </div>
  );
}

function MilestoneRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between items-baseline py-1.5 border-b border-border last:border-0">
      <span className="text-sm text-muted">{label}</span>
      <span className="text-sm text-white tabular-nums font-semibold">{value}</span>
    </div>
  );
}

function TracksCard({ tracks }: { tracks: TrackStat[] }) {
  return (
    <div className="bg-panel border border-border rounded-lg p-3.5 flex flex-col overflow-hidden">
      <h2 className="text-[13px] text-muted uppercase tracking-wider mb-2.5">
        Tracks Visited
      </h2>
      {tracks.length === 0 ? (
        <div className="text-xs text-muted py-4 text-center">No tracks yet</div>
      ) : (
        <div className="overflow-y-auto flex-1">
          <table className="w-full text-sm">
            <thead className="text-[11px] text-muted uppercase tracking-wider">
              <tr>
                <th className="text-left font-normal pb-1.5">Track</th>
                <th className="text-right font-normal pb-1.5">Sessions</th>
                <th className="text-right font-normal pb-1.5">Laps</th>
                <th className="text-right font-normal pb-1.5">Best Lap</th>
              </tr>
            </thead>
            <tbody>
              {tracks.map((t) => (
                <tr key={t.track_id} className="border-t border-border">
                  <td className="py-1.5 text-text">{t.name}</td>
                  <td className="py-1.5 text-right tabular-nums">{t.sessions}</td>
                  <td className="py-1.5 text-right tabular-nums">{t.total_laps}</td>
                  <td className="py-1.5 text-right tabular-nums text-accent">
                    {formatLapMs(t.best_lap_ms)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function MilestonesCard({ stats }: { stats: CareerStats }) {
  return (
    <div className="bg-panel border border-border rounded-lg p-3.5 flex flex-col overflow-hidden">
      <h2 className="text-[13px] text-muted uppercase tracking-wider mb-2.5">Milestones</h2>
      <div className="flex-1 overflow-y-auto">
        <MilestoneRow label="Podiums" value={stats.podiums.toString()} />
        <MilestoneRow label="Fastest laps" value={stats.fastest_laps_earned.toString()} />
        <MilestoneRow
          label="Top speed"
          value={`${stats.top_speed_kmh.toFixed(1)} km/h`}
        />
        <MilestoneRow
          label="Max lateral G"
          value={`${stats.max_g_lateral.toFixed(2)} g`}
        />
        <MilestoneRow
          label="Max braking G"
          value={`${stats.max_g_braking.toFixed(2)} g`}
        />
        <MilestoneRow label="Collisions" value={stats.collisions.toString()} />
        <MilestoneRow label="Penalties" value={stats.penalties.toString()} />
        <MilestoneRow label="Retirements" value={stats.retirements.toString()} />
      </div>
    </div>
  );
}

function CompoundDonut({ data }: { data: Record<string, number> }) {
  const entries = Object.entries(data).filter(([, v]) => v > 0);
  const total = entries.reduce((acc, [, v]) => acc + v, 0);

  if (total === 0) {
    return (
      <div className="bg-panel border border-border rounded-lg p-3.5 flex flex-col">
        <h2 className="text-[13px] text-muted uppercase tracking-wider mb-2.5">
          Tyre Mix
        </h2>
        <div className="flex-1 flex items-center justify-center text-xs text-muted">
          No tyre data yet
        </div>
      </div>
    );
  }

  const chartData = entries.map(([name, value]) => ({
    name: COMPOUND_LABEL[name] ?? name,
    key: name,
    value,
  }));

  return (
    <div className="bg-panel border border-border rounded-lg p-3.5 flex flex-col">
      <h2 className="text-[13px] text-muted uppercase tracking-wider mb-2.5">
        Tyre Mix
      </h2>
      <div className="flex-1 min-h-[160px]">
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie
              data={chartData}
              dataKey="value"
              nameKey="name"
              innerRadius="55%"
              outerRadius="85%"
              paddingAngle={2}
              stroke="#0d1117"
            >
              {chartData.map((d) => (
                <Cell key={d.key} fill={COMPOUND_HEX[d.key] ?? '#8b949e'} />
              ))}
            </Pie>
            <Tooltip
              contentStyle={{
                background: '#161b22',
                border: '1px solid #30363d',
                borderRadius: '6px',
                color: '#c9d1d9',
                fontSize: '12px',
              }}
              formatter={(v: number) => [`${v} samples`, '']}
            />
            <Legend
              iconSize={10}
              wrapperStyle={{ fontSize: '11px', color: '#8b949e' }}
            />
          </PieChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}

function RecentSessionsCard({ sessions }: { sessions: RecentSessionStat[] }) {
  return (
    <div className="bg-panel border border-border rounded-lg p-3.5 flex flex-col overflow-hidden">
      <h2 className="text-[13px] text-muted uppercase tracking-wider mb-2.5">
        Recent Sessions
      </h2>
      {sessions.length === 0 ? (
        <div className="text-xs text-muted py-4 text-center">
          No sessions recorded yet
        </div>
      ) : (
        <div className="overflow-y-auto flex-1">
          <table className="w-full text-sm">
            <thead className="text-[11px] text-muted uppercase tracking-wider">
              <tr>
                <th className="text-left font-normal pb-1.5">Track</th>
                <th className="text-left font-normal pb-1.5">Type</th>
                <th className="text-right font-normal pb-1.5">Laps</th>
                <th className="text-right font-normal pb-1.5">Best Lap</th>
                <th className="text-right font-normal pb-1.5">Finish</th>
                <th className="text-right font-normal pb-1.5">When</th>
                <th className="text-right font-normal pb-1.5"></th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr key={s.session_uid} className="border-t border-border">
                  <td className="py-1.5 text-text">{s.track}</td>
                  <td className="py-1.5 text-muted">{s.session_type_name}</td>
                  <td className="py-1.5 text-right tabular-nums">{s.laps}</td>
                  <td className="py-1.5 text-right tabular-nums text-accent">
                    {formatLapMs(s.best_lap_ms)}
                  </td>
                  <td className="py-1.5 text-right tabular-nums">
                    {s.final_position != null ? `P${s.final_position}` : '—'}
                  </td>
                  <td className="py-1.5 text-right text-muted text-xs">
                    {formatDate(s.ended_at)}
                  </td>
                  <td className="py-1.5 text-right">
                    <Link
                      to={`/insights?uid=${encodeURIComponent(s.session_uid)}`}
                      className="text-accent text-xs hover:underline"
                    >
                      Analyze →
                    </Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

export default function Home() {
  const { stats, loading, error } = useCareerStats();

  if (loading) {
    return (
      <div className="h-full overflow-y-auto p-6">
        <h1 className="text-2xl font-bold text-white mb-4">Career Overview</h1>
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted">
          Loading career stats…
        </div>
      </div>
    );
  }

  if (error || !stats) {
    return (
      <div className="h-full overflow-y-auto p-6">
        <h1 className="text-2xl font-bold text-white mb-4">Career Overview</h1>
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-danger">
          Failed to load career stats: {error ?? 'no data'}
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-y-auto p-6">
      <div className="mb-6">
        <h1 className="text-2xl font-bold text-white">Career Overview</h1>
        <p className="text-sm text-muted mt-1">
          Lifetime stats across {stats.total_sessions} session
          {stats.total_sessions === 1 ? '' : 's'}.
        </p>
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3 mb-4">
        <StatCard
          label="Total races"
          value={stats.total_races.toString()}
          sub={`${stats.total_quali_sessions} quali · ${stats.total_practice_sessions} practice`}
        />
        <StatCard label="Total laps" value={stats.total_laps.toLocaleString()} />
        <StatCard
          label="Hours driven"
          value={formatHours(stats.total_drive_seconds)}
          sub={`${stats.total_distance_km.toFixed(0)} km covered`}
        />
        <StatCard
          label="Best finish"
          value={stats.best_finish != null ? `P${stats.best_finish}` : '—'}
          sub={
            stats.average_finish != null
              ? `Avg P${stats.average_finish.toFixed(1)}`
              : undefined
          }
        />
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-[1.5fr_1fr] gap-3 mb-4 min-h-[280px]">
        <TracksCard tracks={stats.tracks_visited} />
        <MilestonesCard stats={stats} />
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-[2fr_1fr] gap-3 min-h-[260px]">
        <RecentSessionsCard sessions={stats.recent_sessions} />
        <CompoundDonut data={stats.tire_compound_distribution} />
      </div>
    </div>
  );
}
