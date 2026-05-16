import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ConfigResponse, SaveConfigPayload } from '../types/config';
import { API_BASE } from '../lib/constants';

export interface UseConfigResult {
  config: ConfigResponse | null;
  loading: boolean;
  error: string | null;
  saving: boolean;
  restarting: boolean;
  save: (patch: Record<string, unknown>) => Promise<void>;
  refresh: () => Promise<void>;
  restart: () => Promise<void>;
  missingRequired: string[];
}

/**
 * useConfigInternal owns the dashboard's view of /api/config. This is the
 * single-instance hook that ConfigProvider mounts ONCE; consumers
 * elsewhere in the tree must read via `useConfig()` from ConfigContext so
 * the schema/values/missingRequired state stays consistent across the
 * banner, the first-launch gate, and the Settings page.
 *
 * Exposes:
 *  - the current schema + values bundle
 *  - a `save(patch)` function that POSTs and refreshes
 *  - `missingRequired`: derived list of Required keys whose value is empty.
 *  - `restart()`: POST /api/server/restart, which on success means the server
 *    will exit within ~200ms; the caller surfaces a "Restarting…" state and
 *    lets the WS reconnect logic snap back.
 */
export function useConfigInternal(): UseConfigResult {
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [restarting, setRestarting] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    // In the .app the dashboard finishes mounting before the embedded
    // telemetry-core has bound :8081, so the very first /api/config can
    // race the boot. Retry a few times with backoff before surfacing an
    // error — once the server is up subsequent navigations re-use the
    // cached config so the cost is only paid on the cold-start window.
    const attempts = [0, 500, 1000, 1500, 2500];
    let lastErr: unknown = null;
    for (let i = 0; i < attempts.length; i++) {
      if (attempts[i] > 0) await new Promise((r) => setTimeout(r, attempts[i]));
      try {
        const r = await fetch(`${API_BASE}/api/config`);
        if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
        const data: ConfigResponse = await r.json();
        setConfig(data);
        setError(null);
        setLoading(false);
        return;
      } catch (e) {
        lastErr = e;
      }
    }
    setError(lastErr instanceof Error ? lastErr.message : String(lastErr));
    setLoading(false);
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const save = useCallback(
    async (patch: Record<string, unknown>) => {
      setSaving(true);
      try {
        const body: SaveConfigPayload = { patch };
        const r = await fetch(`${API_BASE}/api/config`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        if (!r.ok) {
          const err = await r.json().catch(() => ({}));
          throw new Error(err.error ?? `${r.status} ${r.statusText}`);
        }
        await refresh();
      } finally {
        setSaving(false);
      }
    },
    [refresh],
  );

  const restart = useCallback(async () => {
    setRestarting(true);
    try {
      const r = await fetch(`${API_BASE}/api/server/restart`, { method: 'POST' });
      if (!r.ok && r.status !== 202) {
        const err = await r.json().catch(() => ({}));
        throw new Error(err.error ?? `${r.status} ${r.statusText}`);
      }
    } finally {
      // The server is exiting; useWebSocket will drop and reconnect. We don't
      // clear `restarting` here — the next /api/config refresh that succeeds
      // implies the new process is healthy. Reset after a generous timeout
      // so the button doesn't stay disabled forever if the supervisor isn't
      // running (Phase 1) and the user has to re-run `make start` manually.
      window.setTimeout(() => setRestarting(false), 10_000);
    }
  }, []);

  const missingRequired = useMemo(() => {
    if (!config) return [];
    const out: string[] = [];
    for (const [key, meta] of Object.entries(config.schema)) {
      if (!meta.Required) continue;
      if (isEmpty(config.values[key])) out.push(key);
    }
    return out;
  }, [config]);

  return {
    config,
    loading,
    error,
    saving,
    restarting,
    save,
    refresh,
    restart,
    missingRequired,
  };
}

// isEmpty matches the Go side's notion of an unset value: empty string,
// missing key, or the special "••••" mask returned for unset secrets. A
// secret that has been saved comes back as "••••XXXX" (mask + last 4) so
// `••••` on its own means "no value stored yet".
function isEmpty(v: unknown): boolean {
  if (v == null) return true;
  if (typeof v === 'string') {
    const s = v.trim();
    return s === '' || s === '••••';
  }
  return false;
}
