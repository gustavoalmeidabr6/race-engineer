import { useEffect, useMemo, useRef, useState } from 'react';
import type {
  GridCarView,
  OutlinePoint,
  PlayerPositionView,
  TrackCornerView,
  TrackGeometryView,
} from '../../types/trackPosition';
import { CornerTooltip, type CornerHoverState } from './CornerTooltip';

const SECTOR_COLORS = ['#58a6ff', '#d29922', '#f85149'] as const;

/**
 * Outline samples below this count are treated as cold-start (placeholder
 * ring, no corner snap, fit viewBox to car positions). One or two stray
 * hi-freq samples at lap rollover otherwise produce a pinhole viewBox with
 * cars offscreen and all corner labels stacked on top of each other.
 */
const MIN_USABLE_OUTLINE = 20;

/**
 * Maximum yaw step per render in radians. The parent polls track-position
 * every ~300 ms; clamping the per-tick yaw delta turns abrupt heading jumps
 * (mock teleport, restart, brief packet loss) into a smooth catch-up.
 */
const MAX_YAW_STEP_RAD = 0.5;

interface Props {
  geometry: TrackGeometryView;
  outline: OutlinePoint[];
  me: PlayerPositionView;
  grid: GridCarView[];
}

interface XZ {
  x: number;
  z: number;
}

interface CornerLabel {
  corner: TrackCornerView;
  x: number;
  z: number;
  /** outward normal × label-offset, already in world units */
  ox: number;
  oz: number;
}

/**
 * Pure SVG renderer for the live track map.
 *
 * Coordinates:
 *   F1 25 world is left-handed (x = east, y = up, z = north). Plotting
 *   raw (x, -z) gives a chirality-flipped (mirrored) top-down: clockwise
 *   tracks read counter-clockwise. We negate world.x once at the data
 *   ingress below — every downstream computation (bbox, sampler, car
 *   snap, rotation pivot) then operates in a consistent right-handed
 *   screen frame. All other transforms happen in viewBox space.
 */
export function TrackMapSVG({ geometry, outline, me, grid }: Props) {
  const [hover, setHover] = useState<CornerHoverState | null>(null);
  const svgRef = useRef<SVGSVGElement | null>(null);

  // Mirror world.x once so the resulting top-down matches the F1 25
  // in-game minimap orientation. Cheap (O(N) per poll, ~22 cars + ~250
  // outline points) and keeps every downstream piece coordinate-clean.
  const mirroredOutline = useMemo(
    () => outline.map((p) => ({ ...p, x: -p.x })),
    [outline]
  );
  const mirroredGrid = useMemo(
    () => grid.map((c) => ({ ...c, world: { ...c.world, x: -c.world.x } })),
    [grid]
  );
  const mirroredMe = useMemo(
    () => ({ ...me, world: { ...me.world, x: -me.world.x } }),
    [me]
  );

  // Treat an under-populated outline as no outline at all — see
  // MIN_USABLE_OUTLINE for the reasoning.
  const usableOutline =
    mirroredOutline.length >= MIN_USABLE_OUTLINE ? mirroredOutline : [];

  // ── Compute viewBox ──────────────────────────────────────────────────────
  // Prefer outline bbox (stable per session). Fall back to grid bbox when
  // outline is empty (cold start, < 1 completed lap).
  const bbox = useMemo(
    () => computeBBox(usableOutline, mirroredGrid),
    [usableOutline, mirroredGrid]
  );

  // ── Sector-coloured outline ─────────────────────────────────────────────
  const sectorPaths = useMemo(
    () => buildSectorPaths(usableOutline, geometry.sector_starts_m ?? []),
    [usableOutline, geometry.sector_starts_m]
  );

  // ── Outline sampler: (lap_distance_m) → {x, z, nx, nz} along the line ───
  // Shared by corner labels AND car-dot snapping + cluster spread.
  const sampler = useMemo(
    () => buildOutlineSampler(usableOutline),
    [usableOutline]
  );

  // ── Corner label positions (snap to outline by lap_distance_m) ──────────
  const cornerLabels = useMemo(
    () => snapCornersToOutline(geometry.corners ?? [], usableOutline, sampler),
    [geometry.corners, usableOutline, sampler]
  );

  // ── Car positions: snap to line + spread clustered cars perpendicular ──
  const carDots = useMemo(
    () =>
      layoutCars(
        mirroredGrid,
        mirroredMe.car_index,
        usableOutline.length > 0 ? sampler : null,
        bbox.scale,
      ),
    [mirroredGrid, mirroredMe.car_index, usableOutline.length, sampler, bbox.scale]
  );

  // ── Heading-aligned rotation ───────────────────────────────────────────
  // Pivot the whole world around the player so the player sits at viewport
  // center and forward points up. Skip rotation entirely when the player
  // has no real world position AND we have no outline — there's nothing to
  // pivot around in that case and applying rotation would jerk the
  // placeholder ring around the origin.
  const playerDot = carDots.find((d) => d.isPlayer);
  const playerPx = playerDot
    ? playerDot.x
    : mirroredMe.world.x !== 0 || mirroredMe.world.z !== 0
      ? mirroredMe.world.x
      : null;
  const playerPy = playerDot
    ? -playerDot.z
    : mirroredMe.world.x !== 0 || mirroredMe.world.z !== 0
      ? -mirroredMe.world.z
      : null;

  const targetYaw = Number.isFinite(mirroredMe.yaw) ? mirroredMe.yaw : 0;
  const [displayYaw, setDisplayYaw] = useState(targetYaw);
  useEffect(() => {
    setDisplayYaw((prev) => stepYaw(prev, targetYaw));
  }, [targetYaw]);

  const canRotate = playerPx !== null && playerPy !== null;
  const yawDeg = (displayYaw * 180) / Math.PI;
  // SVG transform list applies right-to-left: translate player to origin,
  // rotate world by -yaw so player's forward becomes screen-up, then
  // translate back to viewport center.
  const viewCx = bbox.minX + bbox.w / 2;
  const viewCy = bbox.minZ + bbox.h / 2;
  const worldTransform = canRotate
    ? `translate(${viewCx} ${viewCy}) rotate(${-yawDeg}) translate(${-(playerPx as number)} ${-(playerPy as number)})`
    : undefined;

  // Counter-rotate text labels so they stay upright after the world group's
  // rotation. Pivot per-label is the label's own (x, y).
  const counterRotate = canRotate
    ? (lx: number, ly: number) => `rotate(${yawDeg} ${lx} ${ly})`
    : () => undefined;

  // ── Render ──────────────────────────────────────────────────────────────
  return (
    <div className="relative w-full h-full">
      <svg
        ref={svgRef}
        viewBox={`${bbox.minX} ${bbox.minZ} ${bbox.w} ${bbox.h}`}
        preserveAspectRatio="xMidYMid meet"
        className="w-full h-full"
      >
       <g transform={worldTransform}>
        {/* Cold-start placeholder: dashed ring sized to the grid bbox */}
        {usableOutline.length === 0 && (
          <>
            <circle
              cx={bbox.minX + bbox.w / 2}
              cy={bbox.minZ + bbox.h / 2}
              r={Math.min(bbox.w, bbox.h) / 2.4}
              fill="none"
              stroke="#444"
              strokeDasharray="20 14"
              strokeWidth={4}
            />
          </>
        )}

        {/* Sector-coloured outline segments */}
        {sectorPaths.map((path, i) => (
          <polyline
            key={i}
            points={path.points}
            fill="none"
            stroke={SECTOR_COLORS[path.sector]}
            strokeWidth={Math.max(bbox.scale * 1.2, 2)}
            strokeLinecap="round"
            strokeLinejoin="round"
            opacity={0.85}
          />
        ))}

        {/* Sector boundary markers */}
        {(geometry.sector_starts_m ?? []).map((s, i) => {
          if (i === 0 || usableOutline.length === 0) return null;
          const p = sampleOutlineAt(usableOutline, s);
          if (!p) return null;
          return (
            <g key={`sb-${i}`}>
              <circle
                cx={p.x}
                cy={-p.z}
                r={Math.max(bbox.scale * 1.4, 3)}
                fill="#0d1117"
                stroke="#fff"
                strokeWidth={Math.max(bbox.scale * 0.4, 1)}
              />
              <g transform={counterRotate(p.x, -p.z + bbox.scale * 0.6)}>
                <text
                  x={p.x}
                  y={-p.z + bbox.scale * 0.6}
                  fontSize={bbox.scale * 4}
                  fontWeight="700"
                  textAnchor="middle"
                  fill="#fff"
                >
                  S{i + 1}
                </text>
              </g>
            </g>
          );
        })}

        {/* Corner badges + invisible hover hit-zones */}
        {cornerLabels.map((cl) => (
          <g
            key={cl.corner.id}
            onMouseEnter={(e) =>
              setHover({
                corner: cl.corner,
                clientX: e.clientX,
                clientY: e.clientY,
              })
            }
            onMouseLeave={() => setHover(null)}
            style={{ cursor: 'pointer' }}
          >
            {/* Tick connecting corner to badge */}
            <line
              x1={cl.x}
              y1={-cl.z}
              x2={cl.x + cl.ox}
              y2={-(cl.z + cl.oz)}
              stroke="#888"
              strokeWidth={Math.max(bbox.scale * 0.3, 0.6)}
            />
            <circle
              cx={cl.x + cl.ox}
              cy={-(cl.z + cl.oz)}
              r={Math.max(bbox.scale * 3.5, 7)}
              fill="#0d1117"
              stroke="#888"
              strokeWidth={Math.max(bbox.scale * 0.4, 1)}
            />
            <g transform={counterRotate(cl.x + cl.ox, -(cl.z + cl.oz) + bbox.scale * 1.3)}>
              <text
                x={cl.x + cl.ox}
                y={-(cl.z + cl.oz) + bbox.scale * 1.3}
                fontSize={bbox.scale * 3.6}
                fontWeight="700"
                textAnchor="middle"
                fill="#fff"
                style={{ pointerEvents: 'none' }}
              >
                {cl.corner.id}
              </text>
            </g>
          </g>
        ))}

        {/* Car dots — snapped to outline + spread away from each other.
            Non-player dots are intentionally translucent so the racing line
            and corner badges underneath stay readable in dense clusters. */}
        {carDots.map((d) => (
          <g key={d.car.car_index}>
            <circle
              cx={d.x}
              cy={-d.z}
              r={d.isPlayer ? Math.max(bbox.scale * 2, 4) : Math.max(bbox.scale * 1.5, 3)}
              fill={d.isPlayer ? 'var(--color-accent, #f78166)' : '#c9d1d9'}
              stroke={d.isPlayer ? '#fff' : '#0d1117'}
              strokeWidth={Math.max(bbox.scale * 0.35, 0.6)}
              fillOpacity={d.isPlayer ? 1 : 0.55}
              opacity={d.car.pit_status === 'on track' ? 1 : 0.45}
            >
              <title>{`P${d.car.position} ${d.car.driver_name ?? `#${d.car.car_index}`}`}</title>
            </circle>
            {d.isPlayer && (
              <g transform={counterRotate(d.x, -d.z - bbox.scale * 5.5)}>
                <text
                  x={d.x}
                  y={-d.z - bbox.scale * 5.5}
                  fontSize={bbox.scale * 4}
                  fontWeight="700"
                  textAnchor="middle"
                  fill="#f78166"
                  style={{ pointerEvents: 'none' }}
                >
                  YOU
                </text>
              </g>
            )}
          </g>
        ))}
       </g>
      </svg>

      {hover && <CornerTooltip hover={hover} />}

      {usableOutline.length === 0 && (
        <div className="absolute top-2 left-1/2 -translate-x-1/2 bg-panel/80 border border-border text-muted text-[11px] px-2 py-1 rounded">
          Recording track…
        </div>
      )}
    </div>
  );
}

/**
 * Per-render damping step for yaw. Wraps the delta into [-π, π] (so a tick
 * across the ±π discontinuity takes the short way around) and clamps to
 * MAX_YAW_STEP_RAD so a teleport or quick spin smooths over a few polls
 * instead of snapping.
 */
function stepYaw(prev: number, target: number): number {
  let delta = target - prev;
  while (delta > Math.PI) delta -= 2 * Math.PI;
  while (delta < -Math.PI) delta += 2 * Math.PI;
  if (Math.abs(delta) <= MAX_YAW_STEP_RAD) return target;
  return prev + Math.sign(delta) * MAX_YAW_STEP_RAD;
}

// ─── Geometry helpers ──────────────────────────────────────────────────────

interface BBox {
  minX: number;
  minZ: number;
  w: number;
  h: number;
  /** Heuristic per-pixel scale for sizing stroke widths and labels. */
  scale: number;
}

function computeBBox(outline: OutlinePoint[], grid: GridCarView[]): BBox {
  let minX = Infinity;
  let maxX = -Infinity;
  let minZ = Infinity;
  let maxZ = -Infinity;
  const ingest = (x: number, z: number) => {
    if (!Number.isFinite(x) || !Number.isFinite(z)) return;
    if (x === 0 && z === 0) return; // skip the F1 25 "no data yet" sentinel
    if (x < minX) minX = x;
    if (x > maxX) maxX = x;
    if (z < minZ) minZ = z;
    if (z > maxZ) maxZ = z;
  };
  for (const p of outline) ingest(p.x, p.z);
  if (outline.length === 0) {
    for (const c of grid) ingest(c.world.x, c.world.z);
  }
  if (!Number.isFinite(minX)) {
    // No data at all — return a safe 1000x1000 box centred on origin.
    return { minX: -500, minZ: -500, w: 1000, h: 1000, scale: 1 };
  }
  // Plot (x, -z) — so the view's minZ is -maxZ.
  const padX = (maxX - minX) * 0.05 + 20;
  const padZ = (maxZ - minZ) * 0.05 + 20;
  const vbMinX = minX - padX;
  const vbMinY = -maxZ - padZ;
  const w = maxX - minX + 2 * padX;
  const h = maxZ - minZ + 2 * padZ;
  const scale = Math.max(w, h) / 200; // every 200 viewBox units ≈ 1 visual unit
  return { minX: vbMinX, minZ: vbMinY, w, h, scale };
}

interface SectorPath {
  sector: number; // 0, 1, 2
  points: string;
}

/**
 * Walks the outline (already sorted by lap_distance_m) and emits one
 * polyline per contiguous sector run. Outline points are mostly contiguous
 * by lap_distance, so a simple sequential pass yields clean sector strokes.
 */
function buildSectorPaths(outline: OutlinePoint[], sectorStarts: number[]): SectorPath[] {
  if (outline.length === 0) return [];
  const s2 = sectorStarts[1] ?? Infinity;
  const s3 = sectorStarts[2] ?? Infinity;
  const sectorOf = (d: number): number => (d < s2 ? 0 : d < s3 ? 1 : 2);

  const paths: SectorPath[] = [];
  let cur: number[] = [];
  let curSector = sectorOf(outline[0].lap_distance_m);
  const flush = (sec: number) => {
    if (cur.length < 2) {
      cur = [];
      return;
    }
    const pts: string[] = [];
    for (let i = 0; i < cur.length; i += 2) {
      pts.push(`${cur[i].toFixed(2)},${cur[i + 1].toFixed(2)}`);
    }
    paths.push({ sector: sec, points: pts.join(' ') });
    cur = [];
  };
  for (const p of outline) {
    const sec = sectorOf(p.lap_distance_m);
    if (sec !== curSector) {
      // Bridge: include the boundary point in BOTH segments so they meet.
      cur.push(p.x, -p.z);
      flush(curSector);
      curSector = sec;
    }
    cur.push(p.x, -p.z);
  }
  flush(curSector);
  return paths;
}

/**
 * Linear-interpolates the outline trace at a given lap_distance_m. Returns
 * null when the outline is empty or the distance falls outside the recorded
 * range (which can happen near the start/finish line on partial laps).
 */
function sampleOutlineAt(outline: OutlinePoint[], distance: number): XZ | null {
  if (outline.length === 0) return null;
  // Linear scan — outline is ~1000 points sorted by lap_distance_m, cheap.
  for (let i = 0; i < outline.length - 1; i++) {
    const a = outline[i];
    const b = outline[i + 1];
    if (distance >= a.lap_distance_m && distance <= b.lap_distance_m) {
      const span = b.lap_distance_m - a.lap_distance_m;
      const t = span > 0 ? (distance - a.lap_distance_m) / span : 0;
      return {
        x: a.x + (b.x - a.x) * t,
        z: a.z + (b.z - a.z) * t,
      };
    }
  }
  return null;
}

// OutlineSampler turns a lap_distance into a point on the outline plus the
// outward-normal at that point. The normal is the tangent rotated 90°,
// flipped (when possible) to point away from the outline centroid — so the
// dashboard can always offset labels / spread cars to the "outside" of the
// track. Returns null when no outline is available.
type OutlineSampler = ((distance: number) => SnapResult | null) & {
  /** Bounding extent of the outline, in world units. */
  extent: number;
};

interface SnapResult {
  x: number;
  z: number;
  /** outward-normal unit vector (length 1) */
  nx: number;
  nz: number;
}

function buildOutlineSampler(outline: OutlinePoint[]): OutlineSampler {
  if (outline.length < 2) {
    const noop: OutlineSampler = Object.assign(
      (_: number): SnapResult | null => null,
      { extent: 0 }
    );
    return noop;
  }
  // Centroid + extent are stable per outline; precompute once.
  let cx = 0;
  let cz = 0;
  let minX = Infinity;
  let maxX = -Infinity;
  let minZ = Infinity;
  let maxZ = -Infinity;
  for (const p of outline) {
    cx += p.x;
    cz += p.z;
    if (p.x < minX) minX = p.x;
    if (p.x > maxX) maxX = p.x;
    if (p.z < minZ) minZ = p.z;
    if (p.z > maxZ) maxZ = p.z;
  }
  cx /= outline.length;
  cz /= outline.length;
  const extent = Math.max(maxX - minX, maxZ - minZ);

  const fn = (distance: number): SnapResult | null => {
    // Walk to bracketing pair. Outline is sorted by lap_distance_m.
    let lo = 0;
    let bracket = false;
    for (let i = 0; i < outline.length - 1; i++) {
      if (
        outline[i].lap_distance_m <= distance &&
        outline[i + 1].lap_distance_m >= distance
      ) {
        lo = i;
        bracket = true;
        break;
      }
    }
    let a = outline[lo];
    let b = outline[lo + 1] ?? outline[lo];
    // Wrap segment: distance beyond last point or before first point falls
    // onto the segment between last and first.
    if (!bracket) {
      if (distance < outline[0].lap_distance_m || distance > outline[outline.length - 1].lap_distance_m) {
        a = outline[outline.length - 1];
        b = outline[0];
      }
    }
    const span = b.lap_distance_m - a.lap_distance_m;
    const t = span > 0 ? Math.max(0, Math.min(1, (distance - a.lap_distance_m) / span)) : 0;
    const x = a.x + (b.x - a.x) * t;
    const z = a.z + (b.z - a.z) * t;

    let tx = b.x - a.x;
    let tz = b.z - a.z;
    const tlen = Math.hypot(tx, tz) || 1;
    tx /= tlen;
    tz /= tlen;
    let nx = -tz;
    let nz = tx;
    const dx = x - cx;
    const dz = z - cz;
    if (nx * dx + nz * dz < 0) {
      nx = -nx;
      nz = -nz;
    }
    return { x, z, nx, nz };
  };
  return Object.assign(fn, { extent });
}

/**
 * For each curated corner, find its (x, z) on the outline and compute an
 * outward-normal offset so the badge sits clear of the racing line.
 */
function snapCornersToOutline(
  corners: TrackCornerView[],
  outline: OutlinePoint[],
  sampler: OutlineSampler
): CornerLabel[] {
  if (corners.length === 0 || outline.length === 0) return [];
  const labelDist = sampler.extent * 0.04;
  const out: CornerLabel[] = [];
  for (const corner of corners) {
    const snap = sampler(corner.lap_distance_m);
    if (!snap) continue;
    out.push({
      corner,
      x: snap.x,
      z: snap.z,
      ox: snap.nx * labelDist,
      oz: snap.nz * labelDist,
    });
  }
  return out;
}

interface CarDot {
  car: GridCarView;
  isPlayer: boolean;
  x: number;
  z: number;
}

/**
 * Snap each car to its position on the outline by lap_distance_m, then
 * spread clusters of cars that share roughly the same lap distance by
 * offsetting them perpendicular to the local outline normal. The first car
 * in a cluster sits on the line; subsequent cars fan ±k×spacing alternately
 * so an entire grid bunched on the start straight stays readable.
 *
 * When `sampler` is null (cold-start: no usable outline), falls back to raw
 * world (x, z) so cars at least show up somewhere on screen.
 */
function layoutCars(
  grid: GridCarView[],
  playerCarIndex: number,
  sampler: OutlineSampler | null,
  bboxScale: number
): CarDot[] {
  if (!sampler) {
    return grid
      .filter((c) => !(c.world.x === 0 && c.world.z === 0))
      .map((c) => ({
        car: c,
        isPlayer: c.car_index === playerCarIndex,
        x: c.world.x,
        z: c.world.z,
      }));
  }

  // Sort by lap_distance_m so adjacent cars in the loop are also adjacent on
  // the track — the cluster detection then collapses to a single pass.
  const sorted = [...grid].sort(
    (a, b) => (a.lap_distance_m || 0) - (b.lap_distance_m || 0)
  );

  // Cars within 5 m of the previous car (on the same lap) share a cluster.
  const CLUSTER_M = 5;
  // Half the previous spacing so a packed grid stays compact; smaller dots
  // (see render block) keep them legible at this density.
  const dotSpacing = Math.max(bboxScale * 3, 6); // viewBox units per lane
  // Cap the perpendicular fan at ±MAX_LANES; further cars wrap to the next
  // ring so the line doesn't shoot off the map at race start.
  const MAX_LANES = 4;

  const dots: CarDot[] = [];
  let lastDistance = -Infinity;
  let clusterIdx = 0;
  for (const c of sorted) {
    const snap = sampler(c.lap_distance_m);
    if (!snap) continue;

    if (c.lap_distance_m - lastDistance <= CLUSTER_M) {
      clusterIdx++;
    } else {
      clusterIdx = 0;
    }
    lastDistance = c.lap_distance_m;

    // Alternating ±, growing magnitude: 0, +1, -1, +2, -2, +3, -3 …
    // Wrap at MAX_LANES so the visual envelope stays bounded.
    const fanMax = 2 * MAX_LANES; // 0, 1..2N positions before wrap
    const wrapped = clusterIdx === 0 ? 0 : ((clusterIdx - 1) % fanMax) + 1;
    const k = Math.ceil(wrapped / 2);
    const sign = wrapped === 0 ? 0 : wrapped % 2 === 1 ? 1 : -1;
    const offset = sign * k * dotSpacing;

    dots.push({
      car: c,
      isPlayer: c.car_index === playerCarIndex,
      x: snap.x + snap.nx * offset,
      z: snap.z + snap.nz * offset,
    });
  }
  return dots;
}
