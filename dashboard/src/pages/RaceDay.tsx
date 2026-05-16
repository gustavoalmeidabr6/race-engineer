import { useNavigate } from 'react-router-dom';
import { useTelemetryStream } from '../context/WebSocketContext';
import { useSettings } from '../hooks/useSettings';
import { Header } from '../components/Header';
import { LiveTelemetry } from '../components/panels/LiveTelemetry';
import { TireDamage } from '../components/panels/TireDamage';
import { WeatherSession } from '../components/panels/WeatherSession';
import { MockControlPanel } from '../components/panels/MockControlPanel';
import { AgentPanel } from '../components/panels/AgentPanel';

export default function RaceDay() {
  const { state, connected, health } = useTelemetryStream();
  const { settings, saveMockOverrides } = useSettings();
  const navigate = useNavigate();
  const goSettings = () => navigate('/settings');

  if (!state) {
    return (
      <div className="h-full flex flex-col p-3">
        <Header health={health} connected={connected} onSettingsClick={goSettings} />
        <div className="flex-1 flex items-center justify-center">
          <div className="text-center">
            <div className="text-2xl font-bold text-muted mb-2">Waiting for telemetry data...</div>
            <div className="text-sm text-[#555]">
              Start the Go telemetry service and ensure F1 25 is sending data
            </div>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="h-full flex flex-col p-3">
      <Header health={health} connected={connected} onSettingsClick={goSettings} />
      <div className="grid grid-cols-[1.4fr_1fr_1fr_1.1fr] gap-3 flex-grow overflow-hidden">
        <LiveTelemetry data={state} />
        <TireDamage data={state} />
        <WeatherSession data={state} />
        <MockControlPanel
          enabled={settings?.mock_mode ?? false}
          overrides={settings?.mock_overrides}
          onSave={saveMockOverrides}
        />
      </div>
      <div className="mt-3 shrink-0">
        <AgentPanel />
      </div>
    </div>
  );
}
