import { useEffect } from 'react';
import { Routes, Route, Navigate, useLocation, useNavigate } from 'react-router-dom';
import { WebSocketProvider } from './context/WebSocketContext';
import { ConfigProvider, useConfig } from './context/ConfigContext';
import { LiveSessionProvider } from './context/LiveSessionContext';
import { AppShell } from './components/AppShell';
import { RequiredSettingsBanner } from './components/RequiredSettingsBanner';
import { ServerGate } from './components/ServerGate';
import Home from './pages/Home';
import RaceDay from './pages/RaceDay';
import Telemetry from './pages/Telemetry';
import DeepInsights from './pages/DeepInsights';
import LiveDebug from './pages/LiveDebug';
import AnalystTeam from './pages/AnalystTeam';
import Settings from './pages/Settings';
import Help from './pages/Help';

// ONBOARDED_KEY is the localStorage marker we drop after a successful
// first-launch nudge so we don't re-redirect on every reload. The user can
// still navigate to /settings manually any time.
const ONBOARDED_KEY = 're_onboarded';

function FirstLaunchGate() {
  const { config, missingRequired } = useConfig();
  const location = useLocation();
  const navigate = useNavigate();

  useEffect(() => {
    if (!config) return;
    if (missingRequired.length === 0) {
      // Drop the marker as soon as the user has things wired up. Future
      // launches with empty settings (e.g. a wiped config) won't redirect.
      try {
        window.localStorage.setItem(ONBOARDED_KEY, '1');
      } catch {
        // localStorage may be disabled (private mode); the worst case is a
        // redirect every launch until the user fills things in.
      }
      return;
    }
    if (window.localStorage.getItem(ONBOARDED_KEY) === '1') return;
    if (location.pathname.startsWith('/settings')) return;
    navigate('/settings#required', { replace: true });
  }, [config, missingRequired, location.pathname, navigate]);

  return null;
}

function App() {
  // ServerGate blocks providers from mounting until the embedded telemetry-core
  // answers /health. This guarantees every downstream hook (useConfig,
  // useCareerStats, useWebSocket, …) sees a live backend on its first fetch,
  // eliminating the "Failed to load" flash that used to appear during the
  // ~2s .app boot window.
  return (
    <ServerGate>
      <ConfigProvider>
        <WebSocketProvider>
          <LiveSessionProvider>
            <FirstLaunchGate />
            <RequiredSettingsBanner />
            <AppShell>
              <Routes>
                <Route path="/" element={<Home />} />
                <Route path="/race" element={<RaceDay />} />
                <Route path="/telemetry" element={<Telemetry />} />
                <Route path="/insights" element={<DeepInsights />} />
                <Route path="/analyst" element={<AnalystTeam />} />
                <Route path="/debug" element={<LiveDebug />} />
                <Route path="/settings" element={<Settings />} />
                <Route path="/help" element={<Help />} />
                <Route path="*" element={<Navigate to="/" replace />} />
              </Routes>
            </AppShell>
          </LiveSessionProvider>
        </WebSocketProvider>
      </ConfigProvider>
    </ServerGate>
  );
}

export default App;
