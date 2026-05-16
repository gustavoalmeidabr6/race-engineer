import type { TrackCornerView } from '../../types/trackPosition';

export interface CornerHoverState {
  corner: TrackCornerView;
  clientX: number;
  clientY: number;
}

interface Props {
  hover: CornerHoverState;
}

/**
 * Pinned to the cursor via fixed positioning so it can pop out of the SVG
 * without clipping. Offset 14px down-right of the pointer; the surrounding
 * page never has scroll on the live-map view so client coords are good.
 */
export function CornerTooltip({ hover }: Props) {
  const { corner, clientX, clientY } = hover;
  return (
    <div
      className="fixed z-50 pointer-events-none bg-panel border border-border rounded shadow-lg px-2.5 py-1.5 text-[11px] leading-tight"
      style={{ left: clientX + 14, top: clientY + 14 }}
    >
      <div className="text-text font-semibold">
        {corner.id}
        {corner.name && corner.name !== corner.id && (
          <span className="text-muted font-normal"> · {corner.name}</span>
        )}
      </div>
      <div className="text-muted">
        {corner.type || 'corner'} · {Math.round(corner.lap_distance_m)} m
      </div>
    </div>
  );
}
