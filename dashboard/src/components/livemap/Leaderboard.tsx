import type { GridCarView, PlayerPositionView } from '../../types/trackPosition';
import { COMPOUND_COLORS, COMPOUND_NAMES } from '../../lib/constants';

interface Props {
  grid: GridCarView[];
  me: PlayerPositionView;
  /** Track length in metres — needed to lap-wrap the interval calculation. */
  trackLengthM: number;
}

/**
 * Right-edge live grid. <=20 rows — no virtualisation. Player row gets the
 * accent border and font weight so the eye finds it instantly when the
 * column is scrolled near the bottom of the standings.
 *
 * The Interval column shows on-track gap to the car immediately ahead in
 * standings (F1 timing-tower convention) — far more glanceable than
 * "everyone is N seconds behind ME".
 */
export function Leaderboard({ grid, me, trackLengthM }: Props) {
  const sorted = [...grid].sort((a, b) => (a.position || 99) - (b.position || 99));
  const intervals = computeIntervalsToCarAhead(sorted, me, trackLengthM);

  return (
    <div className="h-full flex flex-col bg-panel border border-border rounded">
      <div className="px-3 py-2 border-b border-border text-[11px] uppercase tracking-wider text-muted flex items-center gap-2">
        <span className="flex-1">Standings</span>
        <span className="text-text">{sorted.length}</span>
      </div>
      <div className="flex-1 overflow-y-auto">
        <table className="w-full text-[12px] border-collapse">
          <thead className="sticky top-0 bg-panel border-b border-border z-10">
            <tr className="text-[10px] uppercase tracking-wider text-muted">
              <th className="px-2 py-1.5 text-left w-[34px]">Pos</th>
              <th className="px-2 py-1.5 text-left">Driver</th>
              <th className="px-1 py-1.5 text-right">Interval</th>
              <th className="px-1 py-1.5 text-center w-[40px]">Tire</th>
              <th className="px-1 py-1.5 text-center w-[34px]">Pit</th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((c, i) => {
              const isPlayer = c.car_index === me.car_index;
              const compoundColor = COMPOUND_COLORS[c.actual_tyre_compound] ?? '#444';
              const compoundLabel = COMPOUND_NAMES[c.actual_tyre_compound] ?? '?';
              const interval = i === 0 ? 'Leader' : formatInterval(intervals[i]);
              const inPit = c.pit_status !== 'on track';
              const ageLaps = c.tyres_age_laps || 0;
              return (
                <tr
                  key={c.car_index}
                  className={`border-b border-border/40 ${
                    isPlayer
                      ? 'bg-bg/60 border-l-2 border-l-accent'
                      : 'hover:bg-bg/30'
                  }`}
                >
                  <td
                    className={`px-2 py-1.5 font-mono ${
                      isPlayer ? 'text-accent font-bold' : 'text-text'
                    }`}
                  >
                    P{c.position || '-'}
                  </td>
                  <td
                    className={`px-2 py-1.5 truncate max-w-[110px] ${
                      isPlayer ? 'text-accent font-semibold' : 'text-text'
                    }`}
                  >
                    {c.driver_name || `#${c.car_index}`}
                  </td>
                  <td className="px-1 py-1.5 text-right font-mono text-[11px] text-muted">
                    {interval}
                  </td>
                  <td className="px-1 py-1.5 text-center">
                    <span
                      className="inline-flex items-center justify-center min-w-[26px] h-5 px-1 rounded-full text-[10px] font-bold"
                      style={{
                        backgroundColor: compoundColor,
                        color: c.actual_tyre_compound === 18 ? '#000' : '#fff',
                      }}
                      title={`Compound: ${compoundLabel} · age ${ageLaps} lap${ageLaps === 1 ? '' : 's'}`}
                    >
                      {compoundLabel}
                      <span className="opacity-70 ml-0.5">·{ageLaps}</span>
                    </span>
                  </td>
                  <td className="px-1 py-1.5 text-center">
                    {inPit ? (
                      <span
                        className="inline-block text-[10px] px-1 py-0.5 rounded bg-accent/20 text-accent font-semibold"
                        title={c.pit_status}
                      >
                        IN
                      </span>
                    ) : (
                      <span className="text-muted text-[11px]">
                        {c.num_pit_stops || 0}
                      </span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/**
 * Compute the interval — gap to the car immediately ahead in standings —
 * for each row in `sorted` (already in P1..PN order). Returned array is
 * parallel to `sorted`; index 0 is unused (the leader has no car ahead).
 *
 * Gaps come from on-track lap-distance deltas wrapped through the lap
 * length, then converted to seconds via a 70 m/s ground-speed proxy
 * (matches the old formula so the magnitudes don't visually jump). When
 * lap_count differs by ≥1, the lap-wrap collapses the on-track distance
 * onto the shorter direction and we treat anything past `>1 LAP` as
 * lapped — surfaced as an "L" suffix.
 */
function computeIntervalsToCarAhead(
  sorted: GridCarView[],
  me: PlayerPositionView,
  trackLengthM: number,
): number[] {
  const out: number[] = new Array(sorted.length).fill(0);
  if (sorted.length < 2) return out;

  // ahead_of_me_m is signed lap-wrapped distance from me; the leader's
  // value pins our zero. For two cars A (ahead in standings) and B
  // (behind), B-trails-A by (A.ahead - B.ahead) on-track.
  // Player's own ahead_of_me_m isn't populated by the backend — compute
  // it as 0 for the row that is the player.
  const aheadOf = (c: GridCarView) =>
    c.car_index === me.car_index ? 0 : c.ahead_of_me_m;

  for (let i = 1; i < sorted.length; i++) {
    const trail = sorted[i];
    const lead = sorted[i - 1];
    let dist = aheadOf(lead) - aheadOf(trail);
    if (trackLengthM > 0) {
      // Re-wrap the difference back into ±lap/2 in case the two values
      // straddle the original wrap boundary.
      const half = trackLengthM / 2;
      while (dist > half) dist -= trackLengthM;
      while (dist < -half) dist += trackLengthM;
    }
    // Detect lapped: if the trailing car is on a clearly earlier lap,
    // collapse to a magnitude < lap and tag with the lap delta.
    const lapDelta = (lead.current_lap || 0) - (trail.current_lap || 0);
    if (lapDelta >= 1) {
      out[i] = -1 * lapDelta; // sentinel: negative integer = laps down
    } else {
      out[i] = dist; // positive metres (trail is behind lead)
    }
  }
  return out;
}

/**
 * Format the interval value: positive metres → "+0.4s" via the 70 m/s
 * approximation; negative integer sentinel from `computeIntervals` → "+1L"
 * style for lapped cars.
 */
function formatInterval(value: number): string {
  if (value <= -1 && Number.isInteger(value)) {
    const laps = -value;
    return `+${laps}L`;
  }
  // 70 m/s ≈ 250 km/h race-pace ground-speed proxy.
  const seconds = value / 70;
  if (seconds <= 0) {
    // Trail car ahead-of-lead means data is briefly stale (e.g. mid-pit
    // on lap rollover). Show absolute value so the column doesn't flash
    // negatives that mean nothing to the driver.
    return `+${Math.abs(seconds).toFixed(1)}s`;
  }
  return `+${seconds.toFixed(1)}s`;
}
