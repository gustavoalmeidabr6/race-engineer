import { useCallback, useEffect, useRef, useState } from 'react';
import { API_BASE } from '../lib/constants';

// Server-side rates — must match telemetry-core/internal/voice/live.go
// (LiveAudioInputRate / LiveAudioOutputRate). Hard-coded here rather
// than fetched from the server because they're protocol constants.
const INPUT_SAMPLE_RATE = 16000;
const OUTPUT_SAMPLE_RATE = 24000;
const WORKLET_URL = '/audio-worklets/pcm-recorder.js';

export type LiveState = 'idle' | 'requesting-mic' | 'connecting' | 'ready' | 'error' | 'superseded';

/** One observable moment in a Gemini-Live tool-call lifecycle. The
 *  server sends two per call: kind=tool_call before invocation (args
 *  populated) and kind=tool_result after (elapsed_ms + result_bytes,
 *  or error). Pair them by call_id for a tool-use timeline. */
export interface LiveToolEvent {
  kind: 'tool_call' | 'tool_result';
  name: string;
  call_id?: string;
  args?: Record<string, unknown>;
  elapsed_ms?: number;
  result_bytes?: number;
  error?: string;
  at: number; // client-side wall-clock when the frame was received, for ordering
}

export interface UseLiveSessionResult {
  state: LiveState;
  /** Last error message that pushed the session into "error". */
  error: string | null;
  /** Server-side STT of the driver's voice — latest fragment. */
  inputText: string;
  /** Server-side caption of the engineer's voice — latest fragment. */
  outputText: string;
  /** Rolling list of tool-call lifecycle events the model is producing.
   *  Capped at the most recent 50 entries client-side. */
  toolEvents: LiveToolEvent[];
  /** Whether the mic is muted (we still keep the worklet running). */
  micMuted: boolean;
  start: () => Promise<void>;
  stop: () => void;
  toggleMute: () => void;
}

/**
 * useLiveSession owns the bidirectional Gemini Live audio bridge.
 *
 *   start() flow:
 *     1. getUserMedia for the mic.
 *     2. Build an AudioContext, register the pcm-recorder worklet.
 *     3. Open a WebSocket to /api/voice/live.
 *     4. Worklet → main thread → WebSocket binary frames (16kHz PCM).
 *     5. WebSocket binary frames (24kHz PCM) → AudioBufferQueue playback.
 *
 *   stop() tears everything down idempotently.
 *
 * The hook keeps every external resource (MediaStream, AudioContext,
 * WebSocket) in refs so React re-renders don't drop them. A single
 * AbortController would have been cleaner, but the browser APIs predate
 * that pattern; manual cleanup is what the WebAudio / WebSocket world
 * forces on us.
 */
export function useLiveSession(): UseLiveSessionResult {
  const [state, setState] = useState<LiveState>('idle');
  const [error, setError] = useState<string | null>(null);
  const [inputText, setInputText] = useState('');
  const [outputText, setOutputText] = useState('');
  const [toolEvents, setToolEvents] = useState<LiveToolEvent[]>([]);
  const [micMuted, setMicMuted] = useState(false);

  // Capture-side audio graph. AudioContext is created on start() and
  // closed on stop(); the worklet posts ArrayBuffer chunks to the main
  // thread which we forward as WebSocket binary frames.
  const captureCtxRef = useRef<AudioContext | null>(null);
  const captureSourceRef = useRef<MediaStreamAudioSourceNode | null>(null);
  const workletRef = useRef<AudioWorkletNode | null>(null);
  const streamRef = useRef<MediaStream | null>(null);

  // Playback-side audio graph. Held in a separate AudioContext at
  // OUTPUT_SAMPLE_RATE so we don't have to resample the model audio.
  // `nextStartTime` tracks the cumulative scheduled start time so
  // back-to-back chunks play without gaps.
  const playbackCtxRef = useRef<AudioContext | null>(null);
  const nextStartTimeRef = useRef<number>(0);

  // WebSocket handle.
  const wsRef = useRef<WebSocket | null>(null);

  // Synchronous re-entry guard. The state-based check in start() can be
  // bypassed when two callers race before React commits the
  // 'requesting-mic' update — both see state==='idle' and proceed,
  // resulting in two MediaStream grabs, two AudioContexts, two
  // WebSockets, and two Gemini Live sessions for the same user. This
  // ref flips synchronously the moment start() is entered so the second
  // caller bails before doing anything observable.
  const startingRef = useRef(false);

  // Track whether the user has navigated away — async cleanup paths
  // shouldn't write to React state after an unmount.
  const aliveRef = useRef(true);
  useEffect(() => () => { aliveRef.current = false; }, []);

  const updateState = (s: LiveState) => { if (aliveRef.current) setState(s); };
  const updateError = (msg: string | null) => { if (aliveRef.current) setError(msg); };

  const stop = useCallback(() => {
    // Reset the start mutex first so an immediate reconnect attempt
    // after stop() (e.g. from the auto-reconnect provider) isn't blocked.
    startingRef.current = false;
    if (workletRef.current) {
      workletRef.current.disconnect();
      workletRef.current.port.onmessage = null;
      workletRef.current = null;
    }
    if (captureSourceRef.current) {
      captureSourceRef.current.disconnect();
      captureSourceRef.current = null;
    }
    if (captureCtxRef.current) {
      void captureCtxRef.current.close();
      captureCtxRef.current = null;
    }
    if (streamRef.current) {
      streamRef.current.getTracks().forEach((t) => t.stop());
      streamRef.current = null;
    }
    if (playbackCtxRef.current) {
      void playbackCtxRef.current.close();
      playbackCtxRef.current = null;
    }
    nextStartTimeRef.current = 0;
    if (wsRef.current) {
      try {
        wsRef.current.send(JSON.stringify({ type: 'bye' }));
      } catch {
        // ignore; the close below tears it down anyway
      }
      wsRef.current.close();
      wsRef.current = null;
    }
    updateState('idle');
  }, []);

  const playPcmChunk = useCallback((buf: ArrayBuffer) => {
    const ctx = playbackCtxRef.current;
    if (!ctx) return;

    const view = new Int16Array(buf);
    const samples = view.length;
    if (samples === 0) return;

    const audioBuffer = ctx.createBuffer(1, samples, OUTPUT_SAMPLE_RATE);
    const channel = audioBuffer.getChannelData(0);
    for (let i = 0; i < samples; i++) {
      channel[i] = view[i] / 0x8000;
    }
    const src = ctx.createBufferSource();
    src.buffer = audioBuffer;
    src.connect(ctx.destination);

    const now = ctx.currentTime;
    // Schedule slightly into the future on the first chunk so the
    // AudioContext has time to flush its pending output, then chain
    // subsequent chunks back-to-back.
    let startAt = nextStartTimeRef.current;
    if (startAt < now + 0.02) startAt = now + 0.02;
    src.start(startAt);
    nextStartTimeRef.current = startAt + audioBuffer.duration;
  }, []);

  const start = useCallback(async () => {
    // Synchronous mutex first — fires BEFORE any React state update so a
    // second caller racing the first one bails immediately rather than
    // booting a parallel Gemini Live session.
    if (startingRef.current) return;
    if (state === 'connecting' || state === 'ready' || state === 'requesting-mic') {
      return;
    }
    startingRef.current = true;
    updateError(null);
    updateState('requesting-mic');

    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
        },
      });
    } catch (e) {
      startingRef.current = false;
      updateError(e instanceof Error ? e.message : 'Microphone permission denied');
      updateState('error');
      return;
    }
    streamRef.current = stream;

    // Capture context — we let the browser pick its native rate; the
    // worklet handles resampling to INPUT_SAMPLE_RATE.
    const captureCtx = new AudioContext();
    captureCtxRef.current = captureCtx;
    try {
      await captureCtx.audioWorklet.addModule(WORKLET_URL);
    } catch (e) {
      startingRef.current = false;
      updateError('Failed to load audio worklet: ' + (e instanceof Error ? e.message : String(e)));
      updateState('error');
      stop();
      return;
    }

    const source = captureCtx.createMediaStreamSource(stream);
    captureSourceRef.current = source;
    const worklet = new AudioWorkletNode(captureCtx, 'pcm-recorder', {
      processorOptions: { targetRate: INPUT_SAMPLE_RATE, frameMs: 100 },
    });
    workletRef.current = worklet;
    source.connect(worklet);
    // Worklet output is routed to nowhere (we only consume via port);
    // connecting to destination would echo the mic back through the
    // speakers.

    // Playback context for model audio. Browsers cap the sampleRate
    // you can pass here in practice — Chrome accepts most positive
    // ints, Safari is pickier. Wrap in try so we fall back to the
    // default rate if the explicit one is rejected.
    let playbackCtx: AudioContext;
    try {
      playbackCtx = new AudioContext({ sampleRate: OUTPUT_SAMPLE_RATE });
    } catch {
      playbackCtx = new AudioContext();
    }
    playbackCtxRef.current = playbackCtx;
    nextStartTimeRef.current = 0;

    // Open WebSocket. In Wails prod build the page origin isn't the Go core,
    // so build the absolute ws(s):// URL from API_BASE when it's set.
    const wsUrl = API_BASE
      ? API_BASE.replace(/^http/, 'ws') + '/api/voice/live'
      : `${window.location.protocol === 'https:' ? 'wss:' : 'ws:'}//${window.location.host}/api/voice/live`;
    const ws = new WebSocket(wsUrl);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;
    updateState('connecting');

    ws.onopen = () => {
      // Forward worklet PCM chunks straight to the server.
      worklet.port.onmessage = (e) => {
        if (ws.readyState === WebSocket.OPEN && e.data instanceof ArrayBuffer) {
          ws.send(e.data);
        }
      };
    };
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try {
          const env = JSON.parse(e.data) as {
            type: string;
            data?: {
              text?: string;
              reason?: string;
              state?: string;
              kind?: 'tool_call' | 'tool_result';
              name?: string;
              call_id?: string;
              args?: Record<string, unknown>;
              elapsed_ms?: number;
              result_bytes?: number;
              error?: string;
            };
          };
          if (env.type === 'status' && env.data?.state === 'ready') {
            updateState('ready');
          } else if (env.type === 'status' && env.data?.state === 'superseded') {
            // Another tab took over the Gemini Live slot. Move to
            // 'superseded' so the auto-reconnect provider doesn't
            // immediately kick the new tab back out — see
            // LiveSessionContext.tsx for the suppression rule.
            updateError('Voice active in another tab');
            updateState('superseded');
          } else if (env.type === 'input_text' && env.data?.text) {
            if (aliveRef.current) setInputText(env.data.text);
          } else if (env.type === 'output_text' && env.data?.text) {
            if (aliveRef.current) setOutputText(env.data.text);
          } else if (env.type === 'tool_event' && env.data?.kind && env.data?.name) {
            if (aliveRef.current) {
              const ev: LiveToolEvent = {
                kind: env.data.kind,
                name: env.data.name,
                call_id: env.data.call_id,
                args: env.data.args,
                elapsed_ms: env.data.elapsed_ms,
                result_bytes: env.data.result_bytes,
                error: env.data.error,
                at: Date.now(),
              };
              setToolEvents((prev) => {
                const next = [...prev, ev];
                // Cap at 50 — enough for a few minutes of activity, bounded
                // so a long session doesn't grow this array unbounded.
                return next.length > 50 ? next.slice(next.length - 50) : next;
              });
            }
          } else if (env.type === 'error') {
            updateError(env.data?.reason ?? 'live session error');
            updateState('error');
          }
        } catch {
          // ignore malformed JSON; server is the source of truth
        }
        return;
      }
      if (e.data instanceof ArrayBuffer) {
        playPcmChunk(e.data);
      }
    };
    ws.onerror = () => {
      updateError('WebSocket error');
      updateState('error');
    };
    ws.onclose = () => {
      // Preserve 'error' and 'superseded' — both convey information the
      // auto-reconnect logic needs to NOT trigger a reconnect.
      if (state !== 'error' && state !== 'superseded') updateState('idle');
    };

    // All synchronous setup is complete; further state lives inside the
    // ws/event-handler callbacks. Release the mutex so a future reconnect
    // attempt (after onclose) can run.
    startingRef.current = false;
  }, [state, stop, playPcmChunk]);

  const toggleMute = useCallback(() => {
    setMicMuted((m) => {
      const next = !m;
      workletRef.current?.port.postMessage({ muted: next });
      return next;
    });
  }, []);

  // Clean up on unmount.
  useEffect(() => () => stop(), [stop]);

  return { state, error, inputText, outputText, toolEvents, micMuted, start, stop, toggleMute };
}
