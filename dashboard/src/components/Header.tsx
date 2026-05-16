import type { HealthStatus } from '../types/settings';

interface Props {
  health: HealthStatus | null;
  connected: boolean;
  onSettingsClick: () => void;
}

export function Header({ health, connected, onSettingsClick }: Props) {
  const isMock = health?.mock_mode ?? false;
  const lapHint = !connected && !isMock ? 'Waiting for game...' : null;

  return (
    <div className="flex justify-between items-center border-b border-border pb-2 mb-3">
      <div className="flex items-center gap-2.5">
        <h1 className="text-white text-xl font-bold">Race Day</h1>
        {lapHint && <span className="text-xs text-muted">{lapHint}</span>}
      </div>

      <div className="flex items-center gap-3">
        <button
          onClick={onSettingsClick}
          title="Settings"
          className="bg-transparent border border-border rounded-md text-muted cursor-pointer px-2.5 py-1 text-sm flex items-center hover:bg-panel hover:text-white hover:border-muted transition-all"
        >
          ⚙ Settings
        </button>
      </div>
    </div>
  );
}
