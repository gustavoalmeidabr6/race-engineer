import { useEffect, useState } from 'react';
import type { CareerStats } from '../types/career';
import { API_BASE } from '../lib/constants';

export function useCareerStats() {
  const [stats, setStats] = useState<CareerStats | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    fetch(`${API_BASE}/api/stats/career`)
      .then(async (r) => {
        if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
        return r.json() as Promise<CareerStats>;
      })
      .then((data) => {
        if (!cancelled) {
          setStats(data);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return { stats, loading, error };
}
