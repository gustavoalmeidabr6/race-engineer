/** Format lap time from milliseconds to "M:SS.mmm" or "SS.mmm" */
export function formatLapTime(ms: number): string {
  if (!ms || ms <= 0) return '--';
  const mins = Math.floor(ms / 60000);
  const secs = ((ms % 60000) / 1000).toFixed(3);
  return mins > 0 ? `${mins}:${secs.padStart(6, '0')}` : secs;
}

/** Format session time left (seconds) to "M:SS" */
export function formatTimeLeft(secs: number): string {
  if (secs <= 0) return '0:00';
  const m = Math.floor(secs / 60);
  const s = secs % 60;
  return `${m}:${s.toString().padStart(2, '0')}`;
}

/** Format gear: -1=R, 0=N, 1-8 */
export function formatGear(gear: number): string {
  if (gear === -1) return 'R';
  if (gear === 0) return 'N';
  return gear.toString();
}

/** Format delta ms to "+X.XXXs" or "--" */
export function formatDelta(ms: number): string {
  if (ms === 0) return '--';
  const sign = ms > 0 ? '+' : '';
  return `${sign}${(ms / 1000).toFixed(3)}s`;
}

/** Current time as HH:MM:SS */
export function timestamp(): string {
  const n = new Date();
  return (
    n.getHours().toString().padStart(2, '0') + ':' +
    n.getMinutes().toString().padStart(2, '0') + ':' +
    n.getSeconds().toString().padStart(2, '0')
  );
}
