import { NavLink } from 'react-router-dom';
import { useTelemetryStream } from '../context/WebSocketContext';
import { useSharedLiveSession } from '../context/LiveSessionContext';
import { SESSION_TYPE_NAMES } from '../lib/constants';

interface NavItem {
  to: string;
  label: string;
  icon: string;
  liveDot?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { to: '/', label: 'Home', icon: '⌂' },
  { to: '/race', label: 'Race Day', icon: '◉', liveDot: true },
  { to: '/map', label: 'Live Map', icon: '◎', liveDot: true },
  { to: '/telemetry', label: 'Telemetry', icon: '〰' },
  { to: '/insights', label: 'Deep Insights', icon: '▲' },
  { to: '/analyst', label: 'Analyst Team', icon: '◈' },
  { to: '/debug', label: 'Live Debug', icon: '🐛' },
  { to: '/settings', label: 'Settings', icon: '⚙' },
  { to: '/help', label: 'Help', icon: '?' },
];

export function Sidebar() {
  const { connected, health, state } = useTelemetryStream();
  const live = useSharedLiveSession();
  const isMock = health?.mock_mode ?? false;
  const dotColor = connected ? '#2ea043' : '#f85149';
  const connText = isMock ? 'Mock' : connected ? 'Live' : 'Offline';
  const sessionLabel = state
    ? `${SESSION_TYPE_NAMES[state.session_type] ?? '—'} · L${state.current_lap}`
    : null;

  // Engineer-radio status chip. The session auto-connects + auto-reconnects
  // via LiveSessionProvider; here we just translate state → colour + label.
  // Yellow = connecting / requesting mic, green = ready, red = error or idle.
  const radio = (() => {
    switch (live.state) {
      case 'ready':           return { color: '#2ea043', label: 'Live', title: 'Engineer radio is up' };
      case 'connecting':      return { color: '#d29922', label: 'Connecting…', title: 'Opening session' };
      case 'requesting-mic':  return { color: '#d29922', label: 'Mic permission', title: 'Awaiting browser microphone permission' };
      case 'error':           return { color: '#f85149', label: 'Offline', title: live.error ?? 'Session error — auto-retrying' };
      case 'idle':            return { color: '#888888', label: 'Reconnecting…', title: 'Will retry shortly' };
      default:                return { color: '#888888', label: '—', title: 'Unknown radio state' };
    }
  })();

  return (
    <nav className="w-[240px] shrink-0 bg-panel border-r border-border flex flex-col">
      <div className="px-4 py-4 border-b border-border">
        <div className="text-white font-bold text-base leading-tight">Race Engineer</div>
        <div className="text-[11px] text-muted uppercase tracking-wider mt-0.5">Pit Wall</div>
      </div>

      <div className="flex-1 py-3 overflow-y-auto">
        {NAV_ITEMS.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.to === '/'}
            className={({ isActive }) =>
              `flex items-center gap-3 px-4 py-2.5 text-sm transition-colors ${
                isActive
                  ? 'bg-bg text-white border-l-2 border-accent'
                  : 'text-muted hover:bg-bg hover:text-text border-l-2 border-transparent'
              }`
            }
          >
            <span className="w-4 text-center text-base leading-none opacity-80">{item.icon}</span>
            <span className="flex-1">{item.label}</span>
            {item.liveDot && (
              <span
                className="w-2 h-2 rounded-full"
                style={{ backgroundColor: dotColor }}
                title={connText}
              />
            )}
          </NavLink>
        ))}
      </div>

      <div className="border-t border-border px-4 py-3 text-[11px] text-muted">
        <div className="flex items-center gap-2 mb-1.5">
          <span className="w-2 h-2 rounded-full" style={{ backgroundColor: dotColor }} />
          <span className="text-text font-semibold">{connText}</span>
        </div>
        {sessionLabel && (
          <div className="text-accent font-semibold leading-relaxed">{sessionLabel}</div>
        )}
        <div className="leading-relaxed">
          {isMock ? 'MOCK' : 'REAL'} • {health?.udp_host ?? '0.0.0.0'}:{health?.udp_port ?? 20777}
        </div>
        <div className="leading-relaxed">
          UDP {(health?.udp_mode ?? 'broadcast').toUpperCase()}
        </div>

        {/* Engineer radio status — single chip, click to mute the mic. */}
        <button
          type="button"
          onClick={live.toggleMute}
          title={radio.title}
          className="mt-2 w-full flex items-center gap-2 px-2 py-1.5 rounded-md border border-border hover:border-text/40 transition-colors text-left"
        >
          <span className="w-2 h-2 rounded-full" style={{ backgroundColor: radio.color }} />
          <span className="text-text font-semibold flex-1">Engineer Radio</span>
          <span className="text-[10px] text-muted">{live.micMuted ? 'mic off' : radio.label}</span>
        </button>
      </div>
    </nav>
  );
}
