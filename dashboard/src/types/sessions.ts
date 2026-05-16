export interface SessionListItem {
  session_uid: string;
  track_id: number;
  track_name: string;
  session_type: number;
  session_type_name: string;
  first_seen: string;
  last_seen: string;
  total_laps: number;
  player_car_index: number;
  best_lap_ms: number | null;
  final_position: number | null;
}

export interface LapSummary {
  lap: number;
  lap_time_ms: number;
  sector1_ms: number;
  sector2_ms: number;
  sector3_ms: number;
  valid: boolean;
  position?: number | null;
}

export interface TyreWearSample {
  lap: number;
  wear_fl: number;
  wear_fr: number;
  wear_rl: number;
  wear_rr: number;
}

export interface FuelSample {
  lap: number;
  fuel_in_tank: number;
  fuel_laps_left: number;
}

export interface DamageSample {
  lap: number;
  front_wing_left: number;
  front_wing_right: number;
  rear_wing: number;
  floor: number;
  engine: number;
  gearbox: number;
}

export interface EventSummary {
  timestamp: string;
  code: string;
  label: string;
  vehicle_idx: number;
  detail?: string;
}

export interface SessionSummary {
  session_uid: string;
  track_id: number;
  track_name: string;
  session_type: number;
  session_type_name: string;
  track_length_m: number;
  player_car_index: number;
  started_at: string;
  ended_at: string;
  laps: LapSummary[];
  tyre_wear: TyreWearSample[];
  fuel: FuelSample[];
  damage: DamageSample[];
  events: EventSummary[];
  best_lap_ms: number | null;
  final_position: number | null;
}

// /api/laps/traces response (subset we use).
export interface TraceLap {
  lap: number;
  // session_uid is only set when the lap was fetched cross-session via
  // a `N@<uid>` token. Same-session laps omit it for back-compat with
  // dashboards that key on `lap` alone.
  session_uid?: string;
  lap_label?: string;
  lap_time_ms?: number;
  track_position_bucket_m: number[];
  channels: Record<string, (number | null)[]>;
  sample_counts?: number[];
}

export interface TracesResponse {
  session_uid: string;
  track_id: number;
  track_length_m: number;
  buckets: number;
  bucket_size_m: number;
  laps: TraceLap[];
}

// /api/laps/compare response.
export interface CornerCompare {
  id: string;
  name: string;
  lap_distance_m: number;
  best_brake_point_m?: number;
  your_brake_point_m?: number;
  delta_brake_point_m?: number;
  best_apex_speed_kmh?: number;
  your_apex_speed_kmh?: number;
  delta_apex_speed_kmh?: number;
  best_exit_throttle?: number;
  your_exit_throttle?: number;
  delta_exit_throttle?: number;
  note?: string;
}

export interface CompareResponse {
  session_uid: string;
  track_id: number;
  track_length_m: number;
  lap: number;
  lap_time_ms?: number;
  best_lap: number;
  best_lap_time_ms: number;
  delta_total_ms: number;
  window_before_m: number;
  window_after_m: number;
  corners: CornerCompare[];
  note?: string;
}

// /api/laps/delta response — cumulative time delta vs reference lap per bucket.
export interface LapDeltaResponse {
  session_uid: string;
  track_id: number;
  track_length_m: number;
  buckets: number;
  bucket_size_m: number;
  lap: number;
  lap_time_ms?: number;
  reference_lap: number;
  reference_lap_time_ms?: number;
  distance_m: number[];
  delta_ms: (number | null)[];
}

// /api/laps/list — lightweight roster of completed laps for the picker UI.
export interface LapListItem {
  lap: number;
  lap_time_ms: number;
  sector1_ms: number;
  sector2_ms: number;
  sector3_ms: number;
  valid: boolean;
}

export interface LapListResponse {
  session_uid: string;
  car_index: number;
  best_lap?: number;
  laps: LapListItem[];
}
