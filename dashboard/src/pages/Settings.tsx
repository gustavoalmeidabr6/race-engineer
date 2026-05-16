import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useLocation } from 'react-router-dom';
import { useConfig } from '../context/ConfigContext';
import { Tooltip } from '../components/Tooltip';
import type { ConfigKeyMeta, ConfigResponse } from '../types/config';

interface FieldOption {
  value: string;
  label: string;
}

interface ProviderOptionsRef {
  /** Sibling config key whose value selects which option list to show (e.g. "LLM_PROVIDER"). */
  providerKey: string;
  /** Options keyed by provider value. */
  map: Record<string, FieldOption[]>;
  /** Options shown when the provider key is unset or unknown. */
  fallback?: FieldOption[];
}

interface FieldDef {
  key: string;
  label: string;
  help?: string;
  options?: FieldOption[];
  /**
   * Per-provider option lists. When set, the rendered dropdown swaps based
   * on the live value of `providerKey`. Free-text fallback is preserved by
   * the "(custom — type below)" sentinel option.
   */
  optionsByProvider?: ProviderOptionsRef;
  placeholder?: string;
  /** Optional rich help shown in a hover/focus tooltip next to the label. */
  tooltip?: ReactNode;
}

interface SectionDef {
  id: string;
  title: string;
  description: string;
  fields: FieldDef[];
  /** When set, the section starts open regardless of any other rule. */
  defaultOpen?: boolean;
  /** When set, the open/closed toggle is suppressed entirely. */
  pinnedOpen?: boolean;
}

// TOOLTIPS keeps the long-form copy for required keys out of the SECTIONS
// table so it stays readable. Add new entries here when a field needs
// explanation longer than a one-line `help`.
const TOOLTIPS: Record<string, ReactNode> = {
  LLM_PROVIDER: (
    <>
      Which model backend handles chat, voice, and the analyst. Gemini is the
      default and the only fully-featured option today; Anthropic and OpenAI
      cover the analyst but not voice or Live.
    </>
  ),
  GEMINI_API_KEY: (
    <>
      Required to run the analyst, voice replies, and Gemini Live. Get one at{' '}
      <a
        href="https://aistudio.google.com/app/apikey"
        target="_blank"
        rel="noreferrer"
        className="text-accent underline"
      >
        aistudio.google.com/app/apikey
      </a>
      . Stored only on this machine in <code>~/.race-engineer/config.json</code>,
      never sent anywhere except Google's API.
    </>
  ),
  ANTHROPIC_API_KEY: (
    <>
      Required to run the analyst when LLM provider is set to Anthropic. Get
      one at{' '}
      <a
        href="https://console.anthropic.com/settings/keys"
        target="_blank"
        rel="noreferrer"
        className="text-accent underline"
      >
        console.anthropic.com
      </a>
      . Stored only on this machine.
    </>
  ),
  OPENAI_API_KEY: (
    <>
      Required to run the analyst when LLM provider is set to OpenAI. Get one
      at{' '}
      <a
        href="https://platform.openai.com/api-keys"
        target="_blank"
        rel="noreferrer"
        className="text-accent underline"
      >
        platform.openai.com/api-keys
      </a>
      . Stored only on this machine.
    </>
  ),
  PI_AGENT_MODE: (
    <>
      When <code>on</code>, <code>start.sh</code> launches{' '}
      <code>pi_agent_service.py</code> as a sandboxed Python child process —
      the only escape hatch is the MCP endpoint at the path below. The pi
      agent replaces the legacy in-process Strategy Analyst and reacts to
      driver questions, lap completions, and significant rule-engine events
      (tire cliff, weather, damage, safety car, …). Restart required after
      flipping this — the agent runs alongside the Go core, not inside it.
    </>
  ),
  PI_AGENT_MAX_PRIORITY: (
    <>
      The pi agent's <code>push_insight</code> tool is capped at this
      priority server-side, even when its skill or the LLM tries to escalate
      higher. 3 is the recommended default — keeps the agent from
      monopolising team radio. Driver-initiated queries with{' '}
      <code>urgent=true</code> bypass this cap because the driver already
      asked.
    </>
  ),
};

const SECTIONS: SectionDef[] = [
  {
    id: 'telemetry',
    title: 'Telemetry',
    description: 'UDP source, sample rates, and storage paths.',
    fields: [
      {
        key: 'TELEMETRY_MODE',
        label: 'Mode',
        options: [
          { value: 'real', label: 'Real (UDP)' },
          { value: 'mock', label: 'Mock' },
        ],
        help: 'Live applies immediately.',
      },
      { key: 'TELEMETRY_HOST', label: 'UDP host', placeholder: '0.0.0.0' },
      { key: 'TELEMETRY_PORT', label: 'UDP port', placeholder: '20777' },
      {
        key: 'UDP_MODE',
        label: 'UDP mode',
        options: [
          { value: 'broadcast', label: 'Broadcast' },
          { value: 'unicast', label: 'Unicast' },
        ],
      },
      { key: 'SAMPLE_RATE', label: 'Sample rate (Hz divisor)' },
      { key: 'HIFREQ_SAMPLE_RATE', label: 'Hi-freq sample stride' },
      { key: 'BATCH_SIZE', label: 'DuckDB batch size' },
      { key: 'WS_PUSH_RATE', label: 'WebSocket push rate (Hz)' },
      { key: 'API_PORT', label: 'API port' },
      { key: 'DB_PATH', label: 'DuckDB path' },
      { key: 'WORKSPACE_DIR', label: 'Workspace directory' },
    ],
  },
  {
    id: 'llm',
    title: 'LLM',
    description: 'Provider, model, and API keys for the analyst + voice surfaces.',
    fields: [
      {
        key: 'LLM_PROVIDER',
        label: 'Provider',
        options: [
          { value: 'gemini', label: 'Gemini' },
          { value: 'anthropic', label: 'Anthropic' },
          { value: 'openai', label: 'OpenAI' },
        ],
      },
      {
        key: 'LLM_MODEL',
        label: 'Model override (optional)',
        placeholder: 'leave blank for provider default',
        help: 'Type the exact model ID your chosen provider accepts (e.g. claude-opus-4-7, gpt-4o-mini, gemini-3.1-pro-preview). Empty = SDK default.',
      },
      { key: 'GEMINI_API_KEY', label: 'Gemini API key', placeholder: '••••' },
      { key: 'ANTHROPIC_API_KEY', label: 'Anthropic API key', placeholder: '••••' },
      { key: 'OPENAI_API_KEY', label: 'OpenAI API key', placeholder: '••••' },
    ],
  },
  {
    id: 'voice',
    title: 'Voice',
    description: 'Voice input mode + the prebuilt Gemini TTS voice the engineer speaks with.',
    fields: [
      {
        key: 'VOICE_MODE',
        label: 'Voice input mode',
        options: [
          { value: 'live_only', label: 'Gemini Live only (default)' },
          { value: 'ptt_only', label: 'Push-to-talk only (STT + CommsGate)' },
          { value: 'both', label: 'Both (Live + PTT fallback input)' },
        ],
        help: 'Picks which voice-input path runs at boot. live_only skips STT init and disables /api/voice. ptt_only skips the Gemini Live agent factory. Restart required after change.',
      },
      {
        key: 'TTS_VOICE',
        label: 'TTS voice',
        help: 'Prebuilt Gemini voice name (e.g. Kore, Puck, Charon, Aoede). Default: Kore.',
        placeholder: 'Kore',
      },
    ],
  },
  {
    id: 'ptt',
    title: 'Push-to-Talk',
    description: 'Wheel-button mapping for triggering the radio mic. All fields here apply live — no restart.',
    fields: [
      { key: 'PTT_BUTTON', label: 'Bitmask (hex)', placeholder: '0x00000001' },
      {
        key: 'PTT_MODE',
        label: 'PTT mode',
        options: [
          { value: 'hold', label: 'Hold' },
          { value: 'toggle', label: 'Toggle' },
        ],
      },
      {
        key: 'PTT_TRIGGER',
        label: 'Trigger source',
        options: [
          { value: 'off', label: 'Off (no hardware PTT)' },
          { value: 'button', label: 'Button' },
          { value: 'mfd', label: 'MFD page' },
          { value: 'dual', label: 'Dual button (start/end)' },
        ],
      },
      { key: 'PTT_BUTTON_START', label: 'Start bitmask (dual)', placeholder: '0x00010000' },
      { key: 'PTT_BUTTON_END', label: 'End bitmask (dual)', placeholder: '0x00020000' },
      { key: 'LOG_BUTTONS', label: 'Log every BUTN press' },
    ],
  },
  {
    id: 'gemini_live',
    title: 'Gemini Live',
    description: 'Continuous voice session backed by the Gemini Live API. Enabled when Voice input mode is "live_only" or "both" (see Voice section).',
    fields: [
      { key: 'GEMINI_LIVE_MODEL', label: 'Live model' },
      { key: 'GEMINI_LIVE_BRAIN_MAX_CHARS', label: 'Brain context max chars' },
      { key: 'GEMINI_LIVE_ANALYST_TIMEOUT', label: 'Analyst timeout (s)' },
      { key: 'GEMINI_LIVE_PTT_KEY', label: 'PTT key' },
      { key: 'GEMINI_LIVE_PTT_SOURCE', label: 'PTT source' },
    ],
  },
  {
    id: 'pi_agent',
    title: 'Pi Agent',
    description:
      'Sandboxed background analyst team. Runs as a separate Python process whose only outward channel is the MCP endpoint below. Replaces the legacy in-process Strategy Analyst.',
    fields: [
      {
        key: 'PI_AGENT_MODE',
        label: 'Pi Agent mode',
        options: [
          { value: 'off', label: 'Off' },
          { value: 'on', label: 'On (launch pi_agent_service.py)' },
        ],
        help: 'Restart required. Watch the Analyst Team tab for live activity once enabled.',
      },
      {
        key: 'PI_AGENT_PROVIDER',
        label: 'Provider',
        options: [
          { value: 'anthropic', label: 'Anthropic (recommended)' },
          { value: 'gemini', label: 'Gemini' },
          { value: 'openai', label: 'OpenAI' },
          { value: 'custom', label: 'Custom (OpenAI-compatible)' },
        ],
        help: 'Anthropic is recommended for tool-calling depth. Needs the matching API key in the LLM section above. Pick "Custom" to use any OpenAI chat-completions compatible endpoint (Ollama, vLLM, LiteLLM, OpenRouter, Together, …) — then fill in the Base URL and API key fields below.',
      },
      {
        key: 'PI_AGENT_MODEL',
        label: 'Planner model override',
        placeholder: 'leave blank for provider default',
        help: 'Type the exact model ID for the chosen Pi-Agent provider (e.g. claude-opus-4-7, gpt-4o-mini, gemini-3.1-pro-preview, llama3.1:8b for Ollama). Empty = SDK default. The planner is the cheap orchestrator that picks which specialist to dispatch.',
      },
      {
        key: 'PI_AGENT_SPECIALIST_MODEL',
        label: 'Specialist model override',
        placeholder: 'leave blank to reuse planner model',
        help: 'Empty = same as planner. Type the exact model ID your provider accepts. Specialists do the heavy reasoning per persona (tires / brakes / pace / strategy / weather / energy / driving / responder).',
      },
      {
        key: 'PI_AGENT_BASE_URL',
        label: 'Custom base URL',
        placeholder: 'http://localhost:11434/v1',
        help: 'Only used when Provider = Custom. The OpenAI-compatible endpoint your specialist talks to. Examples: http://localhost:11434/v1 (Ollama), http://localhost:8000/v1 (vLLM / LiteLLM), https://openrouter.ai/api/v1 (OpenRouter).',
      },
      {
        key: 'PI_AGENT_API_KEY',
        label: 'Custom API key',
        placeholder: '••••',
        help: 'API key sent to the custom endpoint. Many local servers (Ollama, vLLM) accept any non-empty string and you can leave this blank — a placeholder is sent automatically.',
      },
      {
        key: 'PI_AGENT_MAX_PRIORITY',
        label: 'Max push_insight priority (1–5)',
        help: 'Server-side cap on radio messages the agent can push. 3 keeps it polite.',
      },
      {
        key: 'PI_AGENT_MAX_CONCURRENT_RUNS',
        label: 'Max parallel runs',
        help: 'Cap on specialist runs in flight at once. Each pulled trigger (query / lap / event) spawns its own task; this stops a slow analysis from starving the queue. 4 is a sane default; bump it if you frequently see triggers backing up.',
      },
      {
        key: 'PI_AGENT_MAX_STEPS',
        label: 'Max steps per run',
        help: 'How many LLM round-trips a single specialist run can make before the loop is forced to exit. Each step = one model call (which may issue several tool calls in parallel). 100 leaves a lot of headroom; lower it to enforce terser runs.',
      },
      {
        key: 'PI_AGENT_MCP_PATH',
        label: 'MCP mount path',
        placeholder: '/mcp',
        help: 'HTTP path the Go server mounts the MCP transport on. Match the value the Python child connects to.',
      },
      {
        key: 'PI_AGENT_TRIGGER_TIMEOUT_SEC',
        label: 'Trigger long-poll timeout (s)',
        help: '10s default keeps the planner cheap. Lower = snappier dispatch on idle ticks; higher = fewer wakeups.',
      },
    ],
  },
  {
    id: 'coaching',
    title: 'Coaching',
    description: 'How chatty and verbose your engineer is.',
    fields: [
      { key: 'TALK_LEVEL', label: 'Talk level (1–10)' },
      { key: 'VERBOSITY', label: 'Verbosity (1–10)' },
    ],
  },
  {
    id: 'logging',
    title: 'Transcript & Logging',
    description: 'Log level and transcript retention knobs.',
    fields: [
      {
        key: 'LOG_LEVEL',
        label: 'Log level',
        options: [
          { value: 'trace', label: 'trace' },
          { value: 'debug', label: 'debug' },
          { value: 'info', label: 'info' },
          { value: 'warn', label: 'warn' },
          { value: 'error', label: 'error' },
        ],
      },
      { key: 'TRANSCRIPT_PROMPT_LINES', label: 'Prompt lines injected' },
      { key: 'TRANSCRIPT_TOOL_LIMIT', label: 'Tool API default cap' },
      { key: 'TRANSCRIPT_RETENTION_SESSIONS', label: 'Sessions retained' },
    ],
  },
  {
    id: 'audio',
    title: 'Audio',
    description:
      'Lower the volume of external music players (Spotify, Apple Music, browsers, VLC) while the engineer is speaking, then restore. Restart required after changes.',
    fields: [
      {
        key: 'AUDIO_DUCKING_ENABLED',
        label: 'Ducking enabled',
        options: [
          { value: 'false', label: 'Disabled' },
          { value: 'true', label: 'Enabled' },
        ],
        help: 'Off by default. macOS controls Spotify + Music; Windows uses per-process WASAPI / media key; Linux uses pactl + playerctl.',
      },
      {
        key: 'AUDIO_DUCKING_MODE',
        label: 'Behaviour',
        options: [
          { value: 'duck', label: 'Duck volume (song keeps playing, quieter)' },
          { value: 'pause', label: 'Pause song (resume when engineer stops)' },
        ],
        help: 'Pause keeps the song from progressing while the engineer talks, so you don\'t miss any of it. Pause uses AppleScript on macOS, MPRIS playerctl on Linux, and the global media key on Windows.',
      },
      {
        key: 'AUDIO_DUCKING_LEVEL',
        label: 'Ducked volume (0.0 – 1.0)',
        placeholder: '0.3',
        help: 'Fractional volume players drop to during speech. 0.3 = 30%. Ignored when Behaviour is Pause.',
      },
      {
        key: 'AUDIO_DUCKING_TARGETS',
        label: 'Target apps',
        placeholder: 'leave blank for OS defaults',
        help: 'Comma-separated app names. macOS: Spotify,Music. Windows: spotify.exe,chrome.exe,firefox.exe,msedge.exe,vlc.exe. Linux: spotify,firefox,chromium,vlc.',
      },
      {
        key: 'AUDIO_DUCKING_TAIL_MS',
        label: 'Tail (ms)',
        help: 'How long the duck holds after the last Gemini Live audio chunk. Larger = fewer false toggles between chunks, slower restore.',
      },
    ],
  },
];

function valueForInput(v: unknown, type: ConfigKeyMeta['Type']): string {
  if (v == null) return '';
  if (type === 'bool') return String(Boolean(v));
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

function coerceForSubmit(raw: string, type: ConfigKeyMeta['Type']): unknown {
  if (raw === '' || raw == null) return null;
  if (type === 'int') {
    const n = Number(raw);
    return Number.isFinite(n) ? Math.trunc(n) : raw;
  }
  if (type === 'bool') return raw === 'true' || raw === '1';
  return raw;
}

// Debounce windows: longer for secrets so paste-and-go works without
// firing partial saves while the user is mid-typing an API key.
const DEBOUNCE_MS = 400;
const SECRET_DEBOUNCE_MS = 1200;

type FieldStatus = 'idle' | 'saving' | 'saved' | 'error';

interface FieldInputProps {
  field: FieldDef;
  meta: ConfigKeyMeta;
  serverValue: string;
  isSecret: boolean;
  isFile: boolean;
  onSave: (value: unknown) => Promise<void>;
  /** Resolved option list when `field.optionsByProvider` is active. */
  dynamicOptions?: FieldOption[] | null;
}

/**
 * FieldInput owns one row in the Settings page. It holds a local copy of the
 * value so typing stays responsive while a save is in flight or while
 * /api/config refreshes; once the user pauses, the debounced save fires and
 * we surface a per-field status icon (saving/saved/error). The component is
 * deliberately self-contained so the parent Section doesn't need to track
 * dirty state, save buttons, or flash timers.
 *
 * Race handling: the debounce captures the value it intends to send and only
 * clears the "dirty" marker if the user hasn't typed more by the time the
 * POST completes. Without that check, fast typing during an in-flight save
 * could see the trailing edits silently dropped when the post-save refresh
 * resets local back to the server value.
 */
function FieldInput({ field, meta, serverValue, isSecret, isFile, onSave, dynamicOptions }: FieldInputProps) {
  const [local, setLocal] = useState(serverValue);
  const [status, setStatus] = useState<FieldStatus>('idle');
  const [errMsg, setErrMsg] = useState<string | null>(null);
  const dirtyRef = useRef(false);
  const localRef = useRef(local);
  localRef.current = local;

  // When the server value updates and the field is clean (no pending user
  // edits), sync the displayed value. This keeps the input honest after the
  // save round-trip — e.g. hex inputs come back normalised to uppercase.
  useEffect(() => {
    if (!dirtyRef.current) {
      setLocal(serverValue);
    }
  }, [serverValue]);

  const debounceMs = isSecret ? SECRET_DEBOUNCE_MS : DEBOUNCE_MS;

  // Debounced auto-save. Effect re-runs on every keystroke; the cleanup
  // clears any pending timer so only the final one (after the user pauses)
  // actually fires.
  useEffect(() => {
    if (!dirtyRef.current) return;
    // Secrets: never POST the displayed mask back to the server.
    if (isSecret && (local === '' || isMaskedSecret(local))) return;
    if (local === serverValue) {
      dirtyRef.current = false;
      return;
    }

    const valueAtSchedule = local;
    const t = window.setTimeout(async () => {
      setStatus('saving');
      setErrMsg(null);
      try {
        await onSave(coerceForSubmit(valueAtSchedule, meta.Type));
        // Only mark the field clean if the user hasn't typed anything new
        // since this save was scheduled. Otherwise let the next debounce
        // cycle pick up the trailing edits.
        if (localRef.current === valueAtSchedule) {
          dirtyRef.current = false;
        }
        setStatus('saved');
        window.setTimeout(() => {
          setStatus((s) => (s === 'saved' ? 'idle' : s));
        }, 1500);
      } catch (e) {
        setStatus('error');
        setErrMsg(e instanceof Error ? e.message : String(e));
      }
    }, debounceMs);
    return () => window.clearTimeout(t);
  }, [local, serverValue, onSave, meta.Type, debounceMs, isSecret]);

  const handleChange = useCallback((next: string) => {
    dirtyRef.current = true;
    setLocal(next);
    setStatus((s) => (s === 'saved' || s === 'error' ? 'idle' : s));
  }, []);

  const isRequired = !!meta.Required;
  const isEmpty = (local ?? '') === '';
  const tooltip = field.tooltip ?? TOOLTIPS[field.key];

  return (
    <div className="grid grid-cols-[1fr_2fr] gap-3 items-start">
      <div>
        <div className="flex items-center gap-1.5">
          <span className="text-sm text-text font-semibold">{field.label}</span>
          {tooltip && <Tooltip content={tooltip} />}
        </div>
        <div className="text-[11px] text-muted font-mono mt-0.5">{field.key}</div>
        <div className="flex gap-1.5 mt-1 flex-wrap">
          {isRequired && (
            <span
              className={`text-[10px] uppercase tracking-wider rounded px-1.5 py-0.5 border ${
                isEmpty
                  ? 'text-warning border-warning/60 bg-warning/10'
                  : 'text-warning border-warning/30'
              }`}
            >
              {isEmpty ? 'required — missing' : 'required'}
            </span>
          )}
          {meta.Kind === 'static' && (
            <span className="text-[10px] uppercase tracking-wider text-warning border border-warning/30 rounded px-1.5 py-0.5">
              restart
            </span>
          )}
          {meta.Kind === 'live' && (
            <span className="text-[10px] uppercase tracking-wider text-success border border-success/30 rounded px-1.5 py-0.5">
              live
            </span>
          )}
          {isFile && (
            <span className="text-[10px] uppercase tracking-wider text-accent border border-accent/30 rounded px-1.5 py-0.5">
              saved
            </span>
          )}
        </div>
        {field.help && (
          <div className="text-[11px] text-muted mt-1">{field.help}</div>
        )}
      </div>
      <div>
        {renderInput(field, meta, local, handleChange, dynamicOptions)}
        <FieldStatusLine status={status} errMsg={errMsg} />
      </div>
    </div>
  );
}

function FieldStatusLine({ status, errMsg }: { status: FieldStatus; errMsg: string | null }) {
  if (status === 'idle') return null;
  if (status === 'saving') {
    return <div className="text-[11px] text-muted mt-1">Saving…</div>;
  }
  if (status === 'saved') {
    return <div className="text-[11px] text-success mt-1">Saved</div>;
  }
  return (
    <div className="text-[11px] text-danger mt-1" title={errMsg ?? undefined}>
      Save failed — {errMsg ?? 'unknown error'}
    </div>
  );
}

function isMaskedSecret(s: string): boolean {
  return s.startsWith('••••');
}

interface SectionProps {
  section: SectionDef;
  config: ConfigResponse;
  saveKey: (key: string, value: unknown) => Promise<void>;
}

function Section({ section, config, saveKey }: SectionProps) {
  const fields = useMemo(
    () => section.fields.filter((f) => config.schema[f.key]),
    [section.fields, config.schema],
  );
  const [open, setOpen] = useState(
    section.pinnedOpen || section.defaultOpen || section.id === 'telemetry',
  );

  const sectionHasStatic = fields.some((f) => config.schema[f.key].Kind === 'static');
  const headerInteractive = !section.pinnedOpen;

  return (
    <div
      id={`section-${section.id}`}
      className="bg-panel border border-border rounded-lg overflow-hidden"
    >
      <button
        type="button"
        onClick={() => headerInteractive && setOpen((o) => !o)}
        aria-expanded={open}
        className={`w-full flex items-center gap-3 px-4 py-3 text-left transition-colors ${headerInteractive ? 'hover:bg-bg/40' : 'cursor-default'}`}
      >
        <span className="text-muted text-sm w-3">
          {section.pinnedOpen ? '★' : open ? '▾' : '▸'}
        </span>
        <div className="flex-1">
          <div className="text-sm font-bold text-white">{section.title}</div>
          <div className="text-xs text-muted">{section.description}</div>
        </div>
      </button>

      {open && (
        <div className="border-t border-border p-4 space-y-3">
          {fields.length === 0 && (
            <div className="text-xs text-muted">No fields exposed for this section.</div>
          )}
          {fields.map((field) => {
            const meta = config.schema[field.key];
            const serverValue = valueForInput(config.values[field.key], meta.Type);
            const isFile = config.file_keys.includes(field.key);
            const isSecret = field.key.endsWith('_API_KEY');
            // Resolve per-provider model options from the live config value
            // of the sibling provider key. Section re-renders whenever
            // config.values updates, so picking a new provider above
            // immediately re-keys the model dropdown below.
            let dynamicOptions: FieldOption[] | null = null;
            if (field.optionsByProvider) {
              const ref = field.optionsByProvider;
              const providerVal = String(config.values[ref.providerKey] ?? '').toLowerCase();
              dynamicOptions = ref.map[providerVal] ?? ref.fallback ?? null;
            }
            return (
              <FieldInput
                key={field.key}
                field={field}
                meta={meta}
                serverValue={serverValue}
                isSecret={isSecret}
                isFile={isFile}
                onSave={(v) => saveKey(field.key, v)}
                dynamicOptions={dynamicOptions}
              />
            );
          })}

          {sectionHasStatic && (
            <div className="pt-3 border-t border-border text-[11px] text-muted">
              Fields tagged "restart" take effect on next server restart.
            </div>
          )}
        </div>
      )}
    </div>
  );
}

const CUSTOM_SENTINEL = '__custom__';

function renderInput(
  field: FieldDef,
  meta: ConfigKeyMeta,
  value: string,
  onChange: (v: string) => void,
  dynamicOptions?: FieldOption[] | null,
) {
  if (meta.Type === 'bool') {
    return (
      <select
        value={value || 'false'}
        onChange={(e) => onChange(e.target.value)}
        className="w-full px-3 py-1.5 bg-bg text-white border border-border rounded-md text-sm focus:outline-none focus:border-accent"
      >
        <option value="true">enabled</option>
        <option value="false">disabled</option>
      </select>
    );
  }
  if (field.options) {
    return (
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full px-3 py-1.5 bg-bg text-white border border-border rounded-md text-sm focus:outline-none focus:border-accent"
      >
        <option value="">(unset — default)</option>
        {field.options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    );
  }
  if (dynamicOptions && dynamicOptions.length > 0) {
    // Per-provider model picker: dropdown of curated IDs + a "(custom)"
    // escape hatch that reveals a free-text input so users can paste any
    // model name the provider accepts.
    const matchesKnown = value === '' || dynamicOptions.some((o) => o.value === value);
    const showCustom = !matchesKnown;
    const selectValue = showCustom ? CUSTOM_SENTINEL : value;
    return (
      <div className="space-y-1.5">
        <select
          value={selectValue}
          onChange={(e) => {
            if (e.target.value === CUSTOM_SENTINEL) {
              // Switching to custom mode: keep whatever was typed previously,
              // or seed an empty string so the input below is editable.
              if (matchesKnown) onChange('');
              return;
            }
            onChange(e.target.value);
          }}
          className="w-full px-3 py-1.5 bg-bg text-white border border-border rounded-md text-sm focus:outline-none focus:border-accent"
        >
          <option value="">(unset — provider default)</option>
          {dynamicOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
          <option value={CUSTOM_SENTINEL}>(custom — type below)</option>
        </select>
        {showCustom && (
          <input
            type="text"
            value={value}
            onChange={(e) => onChange(e.target.value)}
            placeholder={field.placeholder ?? 'paste a model ID'}
            className="w-full px-3 py-1.5 bg-bg text-white border border-border rounded-md text-sm font-mono focus:outline-none focus:border-accent"
          />
        )}
      </div>
    );
  }
  const inputType = meta.Type === 'int' ? 'number' : 'text';
  return (
    <input
      type={inputType}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={field.placeholder}
      className="w-full px-3 py-1.5 bg-bg text-white border border-border rounded-md text-sm font-mono focus:outline-none focus:border-accent"
    />
  );
}

// buildRequiredSection composes a synthetic, always-open section at the top
// of the page that holds every key the server marks Required. It pulls
// `label` / `placeholder` from the matching FieldDef in SECTIONS so labels
// stay consistent with the canonical section below; if a Required key is
// missing from SECTIONS we still surface it with a sensible fallback.
function buildRequiredSection(config: ConfigResponse): SectionDef {
  const allFields = new Map<string, FieldDef>();
  for (const section of SECTIONS) {
    for (const field of section.fields) {
      allFields.set(field.key, field);
    }
  }
  const fields: FieldDef[] = [];
  for (const [key, meta] of Object.entries(config.schema)) {
    if (!meta.Required) continue;
    const base = allFields.get(key) ?? { key, label: key };
    fields.push({ ...base, tooltip: TOOLTIPS[key] ?? base.tooltip });
  }
  fields.sort((a, b) => {
    if (a.key === 'LLM_PROVIDER') return -1;
    if (b.key === 'LLM_PROVIDER') return 1;
    return a.key.localeCompare(b.key);
  });
  return {
    id: 'required',
    title: 'Required setup',
    description:
      'These fields gate the analyst, voice replies, and Gemini Live. The dashboard will keep nagging until each one has a value.',
    fields,
    pinnedOpen: true,
  };
}

export default function Settings() {
  const { config, loading, error, save, restarting, restart } = useConfig();
  const location = useLocation();
  const requiredAnchor = useRef<HTMLDivElement | null>(null);

  // saveKey wraps useConfig.save() so each FieldInput POSTs only its own
  // key. Multiple fields can save concurrently — they don't share a global
  // "saving" gate any more. Errors propagate to the calling FieldInput.
  const saveKey = useCallback(
    async (key: string, value: unknown) => {
      await save({ [key]: value });
    },
    [save],
  );

  useEffect(() => {
    if (!config) return;
    if (location.hash !== '#required') return;
    requiredAnchor.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [config, location.hash]);

  if (!config) {
    return (
      <div className="h-full overflow-y-auto p-6">
        <h1 className="text-2xl font-bold text-white mb-2">Settings</h1>
        <div className="bg-panel border border-border rounded-lg p-8 text-center text-muted">
          {error ? `Failed to load: ${error}` : loading ? 'Loading config…' : 'No config available.'}
        </div>
      </div>
    );
  }

  const requiredSection = buildRequiredSection(config);
  const restartLabel = restarting ? 'Restarting…' : 'Restart system';

  return (
    <div className="h-full overflow-y-auto p-6">
      <div className="mb-6 flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-white">Settings</h1>
          <p className="text-sm text-muted mt-1">
            Persisted to <code className="text-accent font-mono">{config.path}</code>.
            Changes save automatically as you type.
          </p>
        </div>
        <button
          type="button"
          onClick={() => void restart()}
          disabled={restarting}
          className="px-3 py-1.5 text-xs font-bold rounded-md border border-warning/60 text-warning hover:bg-warning hover:text-bg disabled:opacity-40 transition-colors"
          title="Drops a workspace/.restart sentinel, exits cleanly, and lets the supervisor bring everything back up. Fields tagged 'restart' take effect after this."
        >
          {restartLabel}
        </button>
      </div>

      <div className="space-y-3">
        <div ref={requiredAnchor}>
          {requiredSection.fields.length > 0 ? (
            <Section section={requiredSection} config={config} saveKey={saveKey} />
          ) : (
            <div className="bg-panel border border-success/40 rounded-lg px-4 py-3 text-sm text-success">
              All required settings are filled in. The dashboard is fully wired.
            </div>
          )}
        </div>
        {SECTIONS.map((s) => (
          <Section key={s.id} section={s} config={config} saveKey={saveKey} />
        ))}
      </div>
    </div>
  );
}
