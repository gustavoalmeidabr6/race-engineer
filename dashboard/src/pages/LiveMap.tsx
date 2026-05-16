import { useTrackPosition } from '../hooks/useTrackPosition';
import { useTrackOutline } from '../hooks/useTrackOutline';
import { TrackMapSVG } from '../components/livemap/TrackMapSVG';
import { Leaderboard } from '../components/livemap/Leaderboard';

/**
 * Live Map page — 2D top-down view of all cars on the current track, with
 * sector-coloured outline, curated corner badges, and a leaderboard.
 *
 * Two data sources:
 *   - /api/state/track_position polled at 300ms (in-memory, cheap)
 *   - /api/track/outline fetched once per session_uid (DuckDB, slow path)
 */
export default function LiveMap() {
  const { data, error } = useTrackPosition(300);
  const { data: outline, loading: outlineLoading } = useTrackOutline(
    data?.track.track_id,
    data?.headline ? extractSessionUid(data) : undefined
  );

  if (error && !data) {
    return (
      <div className="h-full flex items-center justify-center text-muted">
        <div className="text-center">
          <div className="text-lg mb-2">No telemetry yet</div>
          <div className="text-[12px]">{error}</div>
        </div>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="h-full flex items-center justify-center text-muted">
        Waiting for telemetry…
      </div>
    );
  }

  return (
    <div className="h-full flex gap-4 p-4">
      <div className="flex-1 flex flex-col min-w-0">
        <div className="flex items-center gap-3 mb-3">
          <div className="text-text font-bold text-lg">
            {data.track.name || `Track #${data.track.track_id}`}
          </div>
          <div className="text-muted text-[12px]">
            {Math.round(data.track.length_m)} m · {data.track.corners.length} corners
          </div>
          <div className="flex-1" />
          <div className="text-[11px] text-muted">
            {outlineLoading
              ? 'Loading outline…'
              : outline?.points.length
                ? `${outline.points.length} pts${outline.note ? ` · ${outline.note}` : ''}`
                : 'No outline yet'}
          </div>
        </div>

        <div className="flex-1 bg-panel border border-border rounded relative min-h-0">
          <TrackMapSVG
            geometry={data.track}
            outline={outline?.points ?? []}
            me={data.me}
            grid={data.grid}
          />

          {/* Sector legend */}
          <div className="absolute bottom-2 left-2 flex items-center gap-3 text-[11px] text-muted bg-bg/70 border border-border rounded px-2 py-1">
            <LegendDot color="#58a6ff" label="S1" />
            <LegendDot color="#d29922" label="S2" />
            <LegendDot color="#f85149" label="S3" />
            <span className="opacity-60">·</span>
            <span>Player marked YOU</span>
          </div>
        </div>
      </div>

      <div className="w-[320px] shrink-0">
        <Leaderboard
          grid={data.grid}
          me={data.me}
          trackLengthM={data.track.length_m}
        />
      </div>
    </div>
  );
}

function LegendDot({ color, label }: { color: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1">
      <span
        className="inline-block w-2 h-2 rounded-full"
        style={{ backgroundColor: color }}
      />
      {label}
    </span>
  );
}

/**
 * /api/state/track_position doesn't echo the session_uid directly. The Sidebar
 * pulls it from /ws state. For the outline cache key we just need _any_ stable
 * per-session string — `track_id + lap_distance + position-of-leader` rotates
 * with the session, but is fragile. Easier: read window.localStorage for the
 * latest session_uid the WS context wrote, or fall back to track_id alone.
 *
 * Here we use track_id as the cache key — the outline endpoint already
 * resolves its own session by hitting the live cache, so we just want to
 * trigger a re-fetch on track change. Multi-session per-track caching is
 * future work.
 */
function extractSessionUid(_data: unknown): string {
  // Stable per-track key. The outline endpoint internally picks the right
  // session_uid via its own resolve(), so we don't need to pass it.
  return 'live';
}
