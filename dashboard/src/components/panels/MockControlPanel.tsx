import { useState, useEffect } from 'react';
import type { MockOverrides } from '../../types/settings';
import { postMockEvent, type MockEventType } from '../../api/client';

// Buttons grouped by priority lane so they render in the same order the bus
// dispatches them. Critical (P5) → urgent (P4) → routine (P3) → debrief (P2).
const EVENT_BUTTONS: { type: MockEventType; label: string; priority: number; color: string }[] = [
  { type: 'red_flag',         label: 'Red Flag',          priority: 5, color: 'bg-red-900/50 hover:bg-red-900/70 border-red-900/70 text-red-100' },
  { type: 'box_now',          label: 'Box Now',           priority: 5, color: 'bg-red-900/50 hover:bg-red-900/70 border-red-900/70 text-red-100' },
  { type: 'safety_car',       label: 'Safety Car',        priority: 4, color: 'bg-amber-900/40 hover:bg-amber-900/60 border-amber-900/60 text-amber-100' },
  { type: 'vsc',              label: 'VSC',               priority: 4, color: 'bg-amber-900/40 hover:bg-amber-900/60 border-amber-900/60 text-amber-100' },
  { type: 'yellow_flag',      label: 'Yellow Flag',       priority: 4, color: 'bg-amber-900/40 hover:bg-amber-900/60 border-amber-900/60 text-amber-100' },
  { type: 'collision_ahead',  label: 'Collision Ahead',   priority: 4, color: 'bg-amber-900/40 hover:bg-amber-900/60 border-amber-900/60 text-amber-100' },
  { type: 'threat_overtake',  label: 'Threat Behind',     priority: 3, color: 'bg-sky-900/40 hover:bg-sky-900/60 border-sky-900/60 text-sky-100' },
  { type: 'pit_window_open',  label: 'Pit Window',        priority: 3, color: 'bg-sky-900/40 hover:bg-sky-900/60 border-sky-900/60 text-sky-100' },
  { type: 'tire_cliff',       label: 'Tire Cliff',        priority: 3, color: 'bg-sky-900/40 hover:bg-sky-900/60 border-sky-900/60 text-sky-100' },
  { type: 'weather_change',   label: 'Weather Change',    priority: 3, color: 'bg-sky-900/40 hover:bg-sky-900/60 border-sky-900/60 text-sky-100' },
  { type: 'lap_summary',      label: 'Lap Summary',       priority: 2, color: 'bg-zinc-800 hover:bg-zinc-700 border-zinc-700 text-zinc-200' },
  { type: 'fastest_lap',      label: 'Fastest Lap',       priority: 2, color: 'bg-zinc-800 hover:bg-zinc-700 border-zinc-700 text-zinc-200' },
];

interface Props {
  enabled: boolean;
  overrides: MockOverrides | undefined;
  onSave: (overrides: MockOverrides) => void;
}

const WEATHER_OPTIONS = [
  { value: null, label: 'Default' },
  { value: 0, label: 'Clear' },
  { value: 1, label: 'Light Cloud' },
  { value: 2, label: 'Overcast' },
  { value: 3, label: 'Light Rain' },
  { value: 4, label: 'Heavy Rain' },
  { value: 5, label: 'Storm' },
];

export function MockControlPanel({ enabled, overrides, onSave }: Props) {
  const [localOverrides, setLocalOverrides] = useState<MockOverrides>(
    overrides || {
      tire_wear_multiplier: 1,
      fuel_burn_multiplier: 1,
      tire_temp_offset: 0,
      weather_override: null,
      rain_percentage: null,
    }
  );
  const [eventStatus, setEventStatus] = useState<{ type: MockEventType; ok: boolean; msg: string } | null>(null);
  const [pending, setPending] = useState<MockEventType | null>(null);

  const fireEvent = async (type: MockEventType) => {
    if (pending) return;
    setPending(type);
    try {
      const res = await postMockEvent(type);
      setEventStatus({ type, ok: true, msg: `${type} P${res.priority}` });
    } catch (e) {
      const msg = e instanceof Error ? e.message : 'failed';
      setEventStatus({ type, ok: false, msg });
    } finally {
      setPending(null);
      // Auto-dismiss the toast after 2.5s so the panel doesn't stay noisy.
      setTimeout(() => setEventStatus(prev => (prev?.type === type ? null : prev)), 2500);
    }
  };

  useEffect(() => {
    if (overrides) {
      setLocalOverrides(overrides);
    }
  }, [overrides]);

  const handleChange = <K extends keyof MockOverrides>(key: K, value: MockOverrides[K]) => {
    const next = { ...localOverrides, [key]: value };
    setLocalOverrides(next);
    onSave(next);
  };

  if (!enabled) {
    return (
      <div className="bg-panel border border-border rounded-lg p-3.5 opacity-50 grayscale pointer-events-none relative overflow-hidden">
        <h2 className="text-[13px] text-muted uppercase tracking-wider mb-2.5">God Mode Controls</h2>
        <div className="absolute inset-0 flex items-center justify-center bg-black/40 z-10 backdrop-blur-[1px]">
          <span className="text-xs font-bold text-white bg-black/80 px-2 py-1 rounded border border-border">
            Switch to Mock Mode to enable
          </span>
        </div>
        <div className="space-y-4">
           {/* Placeholder content to maintain layout height */}
           <div className="h-10 bg-black/20 rounded" />
           <div className="h-10 bg-black/20 rounded" />
           <div className="h-10 bg-black/20 rounded" />
        </div>
      </div>
    );
  }

  return (
    <div className="bg-panel border border-border rounded-lg p-3.5 flex flex-col">
      <h2 className="text-[13px] text-accent uppercase tracking-wider mb-2.5 font-bold">God Mode Controls</h2>

      <div className="space-y-4 overflow-y-auto pr-1">
        {/* Tire Wear Multiplier */}
        <div>
          <div className="flex justify-between items-center mb-1">
            <label className="text-xs font-bold text-muted">Tire Wear Multiplier:</label>
            <span className="text-xs font-mono text-accent">{localOverrides.tire_wear_multiplier.toFixed(1)}x</span>
          </div>
          <input
            type="range"
            min={0}
            max={10}
            step={0.5}
            value={localOverrides.tire_wear_multiplier}
            onChange={e => handleChange('tire_wear_multiplier', Number(e.target.value))}
            className="w-full accent-accent cursor-pointer h-1.5 bg-black rounded-lg appearance-none"
          />
          <div className="flex justify-between text-[10px] text-[#555] mt-1">
            <span>None</span>
            <span>Normal (1x)</span>
            <span>Extreme (10x)</span>
          </div>
        </div>

        {/* Tire Temp Offset */}
        <div>
          <div className="flex justify-between items-center mb-1">
            <label className="text-xs font-bold text-muted">Tire Temp Offset:</label>
            <span className="text-xs font-mono text-accent">+{localOverrides.tire_temp_offset.toFixed(0)}°C</span>
          </div>
          <input
            type="range"
            min={0}
            max={100}
            step={5}
            value={localOverrides.tire_temp_offset}
            onChange={e => handleChange('tire_temp_offset', Number(e.target.value))}
            className="w-full accent-accent cursor-pointer h-1.5 bg-black rounded-lg appearance-none"
          />
          <div className="flex justify-between text-[10px] text-[#555] mt-1">
            <span>Normal</span>
            <span>Overheating (+100°C)</span>
          </div>
        </div>

        {/* Fuel Burn Multiplier */}
        <div>
          <div className="flex justify-between items-center mb-1">
            <label className="text-xs font-bold text-muted">Fuel Burn Multiplier:</label>
            <span className="text-xs font-mono text-accent">{localOverrides.fuel_burn_multiplier.toFixed(1)}x</span>
          </div>
          <input
            type="range"
            min={0}
            max={5}
            step={0.1}
            value={localOverrides.fuel_burn_multiplier}
            onChange={e => handleChange('fuel_burn_multiplier', Number(e.target.value))}
            className="w-full accent-accent cursor-pointer h-1.5 bg-black rounded-lg appearance-none"
          />
        </div>

        {/* Weather Override */}
        <div className="grid grid-cols-2 gap-2">
          <div>
            <label className="text-[10px] font-bold text-muted uppercase block mb-1">Weather</label>
            <select
              value={localOverrides.weather_override ?? ''}
              onChange={e => handleChange('weather_override', e.target.value === '' ? null : Number(e.target.value))}
              className="w-full bg-black border border-border text-xs p-1.5 rounded focus:outline-none focus:border-accent"
            >
              {WEATHER_OPTIONS.map(opt => (
                <option key={opt.label} value={opt.value ?? ''}>{opt.label}</option>
              ))}
            </select>
          </div>
          <div>
            <label className="text-[10px] font-bold text-muted uppercase block mb-1">Rain %</label>
            <div className="flex items-center gap-2">
              <input
                type="number"
                min={0}
                max={100}
                value={localOverrides.rain_percentage ?? ''}
                placeholder="Auto"
                onChange={e => handleChange('rain_percentage', e.target.value === '' ? null : Number(e.target.value))}
                className="w-full bg-black border border-border text-xs p-1.5 rounded focus:outline-none focus:border-accent"
              />
              <button 
                onClick={() => handleChange('rain_percentage', null)}
                className="text-[10px] bg-border/20 hover:bg-border/40 px-1 rounded h-full"
              >
                Reset
              </button>
            </div>
          </div>
        </div>

        <div className="pt-2 border-t border-border mt-2">
           <div className="flex gap-2">
             <button
               onClick={() => {
                 const reset = {
                   tire_wear_multiplier: 1,
                   fuel_burn_multiplier: 1,
                   tire_temp_offset: 0,
                   weather_override: null,
                   rain_percentage: null,
                 };
                 setLocalOverrides(reset);
                 onSave(reset);
               }}
               className="flex-1 py-1.5 bg-border/20 text-white border-none rounded cursor-pointer text-[11px] font-bold hover:bg-border/40 transition-colors"
             >
               Reset All
             </button>
             <button
               onClick={() => {
                 const extreme = {
                   tire_wear_multiplier: 5,
                   fuel_burn_multiplier: 1,
                   tire_temp_offset: 40,
                   weather_override: 4, // Heavy Rain
                   rain_percentage: 85,
                 };
                 setLocalOverrides(extreme);
                 onSave(extreme);
               }}
               className="flex-1 py-1.5 bg-red-900/40 text-red-200 border border-red-900/60 rounded cursor-pointer text-[11px] font-bold hover:bg-red-900/60 transition-colors"
             >
               Extreme Stress
             </button>
           </div>
        </div>

        <div className="pt-2 border-t border-border mt-2">
          <div className="flex justify-between items-center mb-2">
            <label className="text-[10px] font-bold text-muted uppercase">Event Simulator</label>
            {eventStatus && (
              <span className={`text-[10px] font-mono ${eventStatus.ok ? 'text-accent' : 'text-red-400'}`}>
                {eventStatus.ok ? '✓' : '✗'} {eventStatus.msg}
              </span>
            )}
          </div>
          <div className="grid grid-cols-2 gap-1.5">
            {EVENT_BUTTONS.map(btn => (
              <button
                key={btn.type}
                onClick={() => fireEvent(btn.type)}
                disabled={pending !== null}
                title={`Fire ${btn.type} (P${btn.priority}) onto the Interrupts Bus`}
                className={`py-1 px-1.5 border rounded cursor-pointer text-[10px] font-bold transition-colors disabled:opacity-50 disabled:cursor-not-allowed flex justify-between items-center ${btn.color}`}
              >
                <span className="truncate">{btn.label}</span>
                <span className="text-[9px] font-mono opacity-70 ml-1">P{btn.priority}</span>
              </button>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}
