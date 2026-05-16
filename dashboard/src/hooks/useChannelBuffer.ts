import { useEffect, useRef, useState } from 'react';
import { useTelemetryStream } from '../context/WebSocketContext';

// useChannelBuffer maintains a rolling buffer of RaceState samples for the
// live Telemetry charts. The WebSocket already pushes at WS_PUSH_RATE Hz
// (10 Hz default) so we just dedupe on frame_id and trim to the requested
// time window. Returns a stable React state slice that recharts can
// consume directly without re-allocating per render.

// ChannelFrame is the slim shape we keep in the buffer — one row per WS
// frame, only fields the live charts need. The full RaceState is much
// larger and most fields don't drive the charts.
export interface ChannelFrame {
  t: number; // ms since first sample in this session (X axis)
  frame: number;
  throttle: number;
  brake: number;
  steering: number;
  speed: number;
  gear: number;
  rpm: number;
  drs: number;
  g_lat: number;
  g_lon: number;
  g_vert: number;
  // 4-wheel averages keep the buffer light; the per-wheel arrays still
  // live on `RaceState` for any chart that wants asymmetry.
  brake_temp_avg: number;
  tyre_surf_temp_avg: number;
  // Per-wheel temps so the 4-wheel brake/tyre cards can render asymmetry.
  brake_temp_fl: number;
  brake_temp_fr: number;
  brake_temp_rl: number;
  brake_temp_rr: number;
  tyre_temp_fl: number;
  tyre_temp_fr: number;
  tyre_temp_rl: number;
  tyre_temp_rr: number;
  fuel: number;
  ers_store_pct: number;
  ers_deploy_mode: number;
}

interface Options {
  windowSec: number; // how much history to keep (default 30s)
  maxSamples?: number; // hard cap to keep recharts responsive (default 600)
}

// avg4 averages a 4-element array (RaceState tyre/brake arrays). The F1 25
// packet order is RL/RR/FL/FR but for an average that doesn't matter.
function avg4(arr: number[] | undefined): number {
  if (!arr || arr.length < 4) return 0;
  return (arr[0] + arr[1] + arr[2] + arr[3]) / 4;
}

export function useChannelBuffer({ windowSec, maxSamples = 600 }: Options) {
  const { state } = useTelemetryStream();
  const [frames, setFrames] = useState<ChannelFrame[]>([]);
  const startTimeRef = useRef<number | null>(null);
  const lastFrameIdRef = useRef<number>(-1);

  useEffect(() => {
    if (!state) return;
    // Dedupe on frame_id so duplicate WS pushes (reconnect, multi-listener)
    // don't double-buffer. frame_id resets per session — accept a smaller
    // value when it does, so we don't strand the buffer when SessionUID
    // changes.
    if (state.frame_id === lastFrameIdRef.current) return;
    if (state.frame_id < lastFrameIdRef.current - 1000) {
      // Probable new session; reset.
      startTimeRef.current = null;
      setFrames([]);
    }
    lastFrameIdRef.current = state.frame_id;

    const now = performance.now();
    if (startTimeRef.current == null) startTimeRef.current = now;
    const t = now - startTimeRef.current;

    // F1 25 tyre/brake temp arrays are [RL, RR, FL, FR].
    const bt = state.brakes_temp ?? [0, 0, 0, 0];
    const tt = state.tyres_surface_temp ?? [0, 0, 0, 0];

    const frame: ChannelFrame = {
      t,
      frame: state.frame_id,
      throttle: state.throttle,
      brake: state.brake,
      steering: state.steering,
      speed: state.speed,
      gear: state.gear,
      rpm: state.engine_rpm,
      drs: state.drs,
      g_lat: state.g_force_lateral,
      g_lon: state.g_force_longitudinal,
      g_vert: state.g_force_vertical,
      brake_temp_avg: avg4(bt),
      tyre_surf_temp_avg: avg4(tt),
      brake_temp_rl: bt[0] ?? 0,
      brake_temp_rr: bt[1] ?? 0,
      brake_temp_fl: bt[2] ?? 0,
      brake_temp_fr: bt[3] ?? 0,
      tyre_temp_rl: tt[0] ?? 0,
      tyre_temp_rr: tt[1] ?? 0,
      tyre_temp_fl: tt[2] ?? 0,
      tyre_temp_fr: tt[3] ?? 0,
      fuel: state.fuel_in_tank,
      // ers_store_energy is Joules (0..4_000_000); convert to % for charts.
      ers_store_pct: state.ers_store_energy > 0
        ? Math.min(100, (state.ers_store_energy / 4_000_000) * 100)
        : 0,
      ers_deploy_mode: state.ers_deploy_mode,
    };

    setFrames((prev) => {
      const next = prev.length >= maxSamples ? prev.slice(1) : prev.slice();
      next.push(frame);
      // Trim by window. Use binary search for correctness when many
      // frames have accumulated; linear is fine at 10Hz × 60s = 600 rows.
      const cutoff = t - windowSec * 1000;
      let dropIdx = 0;
      while (dropIdx < next.length && next[dropIdx].t < cutoff) dropIdx++;
      return dropIdx > 0 ? next.slice(dropIdx) : next;
    });
  }, [state, windowSec, maxSamples]);

  return frames;
}
