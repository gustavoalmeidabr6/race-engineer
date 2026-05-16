export interface TrackStat {
  track_id: number;
  name: string;
  sessions: number;
  best_lap_ms: number | null;
  total_laps: number;
}

export interface RecentSessionStat {
  session_uid: string;
  track: string;
  track_id: number;
  session_type_name: string;
  session_type: number;
  ended_at: string;
  laps: number;
  best_lap_ms: number | null;
  final_position: number | null;
}

export interface CareerStats {
  total_sessions: number;
  total_races: number;
  total_quali_sessions: number;
  total_practice_sessions: number;
  total_laps: number;
  total_distance_km: number;
  total_drive_seconds: number;
  best_finish: number | null;
  average_finish: number | null;
  podiums: number;
  fastest_laps_earned: number;
  collisions: number;
  penalties: number;
  retirements: number;
  top_speed_kmh: number;
  max_g_lateral: number;
  max_g_braking: number;
  tire_compound_distribution: Record<string, number>;
  tracks_visited: TrackStat[];
  recent_sessions: RecentSessionStat[];
}
