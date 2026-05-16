/** Mirrors Go telemetry-core/internal/api/track_position_handler.go. */

export interface WorldXYZ {
  x: number;
  y: number;
  z: number;
}

export interface TrackCornerView {
  id: string;
  name: string;
  lap_distance_m: number;
  type: string;
}

export interface TrackCornerWithDistance extends TrackCornerView {
  distance_ahead_m: number;
}

export interface TrackGeometryView {
  name: string;
  track_id: number;
  length_m: number;
  /** [0, sector2_start_m, sector3_start_m] */
  sector_starts_m: number[];
  corners: TrackCornerView[];
  _note?: string;
}

export interface PlayerPositionView {
  car_index: number;
  driver_name?: string;
  position: number;
  lap: number;
  sector: number;
  lap_distance_m: number;
  speed_kmh: number;
  world: WorldXYZ;
  /** Heading in radians. 0 = world +z (north), CCW positive. */
  yaw: number;
  next_corner?: TrackCornerWithDistance;
}

export interface GridCarView {
  car_index: number;
  driver_name?: string;
  position: number;
  lap_distance_m: number;
  current_lap: number;
  sector: number;
  pit_status: string;
  driver_status: string;
  world: WorldXYZ;
  ahead_of_me_m: number;
  actual_tyre_compound: number;
  visual_tyre_compound: number;
  tyres_age_laps: number;
  num_pit_stops: number;
}

export interface TrackPositionResponse {
  headline: string;
  track: TrackGeometryView;
  me: PlayerPositionView;
  grid: GridCarView[];
}

/**
 * Per-tick slice of TrackPositionResponse, pushed over /ws as the
 * `track_position` message. Mirrors Go api.TrackPositionDynamic — omits
 * the static `track` geometry block which is fetched once via REST per
 * session.
 */
export interface TrackPositionDynamic {
  headline: string;
  me: PlayerPositionView;
  grid: GridCarView[];
}

/** Mirrors Go telemetry-core/internal/api/track_outline_handler.go. */
export interface OutlinePoint {
  lap_distance_m: number;
  x: number;
  z: number;
}

export interface TrackOutlineResponse {
  track_id: number;
  session_uid: string;
  track_length_m: number;
  points: OutlinePoint[];
  note?: string;
}
