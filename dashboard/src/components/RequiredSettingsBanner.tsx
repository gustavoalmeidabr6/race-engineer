import { Link, useLocation } from 'react-router-dom';
import { useConfig } from '../context/ConfigContext';

/**
 * Yellow bar shown above the routed page whenever the dashboard knows that a
 * required setting (LLM provider, the chosen provider's API key) is still
 * empty. When the user is already on the Settings page the banner stays
 * passive — no link, just the explanation — so the call to action doesn't
 * become a no-op.
 *
 * The banner reuses the existing `useConfig` hook which already keeps the
 * required-key list fresh against the server; no separate fetch.
 */
export function RequiredSettingsBanner() {
  const { config, missingRequired } = useConfig();
  const location = useLocation();
  if (!config || missingRequired.length === 0) return null;

  const onSettings = location.pathname.startsWith('/settings');
  const list = missingRequired.join(', ');

  return (
    <div
      role="alert"
      className="bg-warning/10 border-b border-warning/40 text-warning px-4 py-2 text-xs flex items-center gap-3"
    >
      <span className="font-bold uppercase tracking-wider">Setup needed</span>
      <span className="text-text">
        Missing required setting{missingRequired.length === 1 ? '' : 's'}:{' '}
        <code className="font-mono text-warning">{list}</code>. The analyst, voice
        replies, and Gemini Live won't work until these are filled in.
      </span>
      {!onSettings && (
        <Link
          to="/settings#required"
          className="ml-auto px-3 py-1 rounded-md border border-warning/60 text-warning hover:bg-warning hover:text-bg font-semibold transition-colors"
        >
          Go to Settings →
        </Link>
      )}
    </div>
  );
}
