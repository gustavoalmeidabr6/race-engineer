// PCM recorder AudioWorklet.
//
// Captures the mic stream from the connected AudioNode, downsamples it
// to 16 kHz mono, converts to PCM-16 little-endian, and posts ~100 ms
// chunks back to the main thread.
//
// Gemini Live expects audio/pcm at 16 kHz mono. The browser's input
// AudioContext usually runs at 48 kHz (Chrome default) or whatever the
// device offers; we downsample here so the rest of the pipeline can
// stay rate-agnostic.
//
// Loaded from /audio-worklets/pcm-recorder.js by the dashboard
// useLiveSession hook. Plain JS (not TS) so Vite serves it verbatim;
// AudioWorklet code runs in a separate JS realm without bundling.

class PCMRecorder extends AudioWorkletProcessor {
  constructor(opts) {
    super();
    const options = (opts && opts.processorOptions) || {};
    this.targetRate = options.targetRate || 16000;
    this.frameMs = options.frameMs || 100;
    // Resampling state for a simple linear-interpolation downsampler.
    // Good enough for speech recognition; if quality matters later swap
    // in a windowed-sinc.
    this.ratio = sampleRate / this.targetRate; // sampleRate is global in worklet scope
    this.fractional = 0;
    this.last = 0;
    this.outBuffer = [];
    this.framesNeeded = Math.round((this.frameMs / 1000) * this.targetRate);
    this.muted = false;

    this.port.onmessage = (e) => {
      if (e.data && typeof e.data === 'object' && 'muted' in e.data) {
        this.muted = !!e.data.muted;
      }
    };
  }

  // process is called every 128 samples. We pull the first input
  // channel, downsample, and accumulate until we have ~100 ms of audio
  // at the target rate, then post it.
  process(inputs) {
    if (this.muted) return true;
    const input = inputs[0];
    if (!input || input.length === 0) return true;
    const channel = input[0];
    if (!channel) return true;

    // Linear-interpolation resampler. We track a fractional sample
    // index that advances by `ratio` every output sample; whenever it
    // crosses an integer boundary we emit a new sample.
    let i = this.fractional;
    while (i < channel.length) {
      const i0 = Math.floor(i);
      const i1 = Math.min(i0 + 1, channel.length - 1);
      const frac = i - i0;
      const sample = channel[i0] * (1 - frac) + channel[i1] * frac;
      this.outBuffer.push(sample);
      i += this.ratio;
    }
    this.fractional = i - channel.length;

    while (this.outBuffer.length >= this.framesNeeded) {
      const slice = this.outBuffer.splice(0, this.framesNeeded);
      const pcm = new Int16Array(slice.length);
      for (let j = 0; j < slice.length; j++) {
        const s = Math.max(-1, Math.min(1, slice[j]));
        pcm[j] = s < 0 ? s * 0x8000 : s * 0x7fff;
      }
      // Post the underlying ArrayBuffer transferred so the main thread
      // can ship it to the WebSocket without copying.
      this.port.postMessage(pcm.buffer, [pcm.buffer]);
    }
    return true;
  }
}

registerProcessor('pcm-recorder', PCMRecorder);
