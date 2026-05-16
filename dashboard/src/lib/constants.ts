// Dev: '' → Vite proxies /api and /ws to the Go core on :8081.
// Prod (Wails .app): absolute origin → React assets are served from the
// embedded assetserver (wails://wails.localhost), so relative URLs would
// never reach the Go core. CORS is wide-open server-side.
export const API_BASE = import.meta.env.PROD ? 'http://localhost:8081' : '';

// --- Tire array reorder: Go sends [RL, RR, FL, FR], display needs [FL, FR, RL, RR] ---
export const reorderTires = <T>(arr: T[]): T[] => [arr[2], arr[3], arr[0], arr[1]];

// --- Enum maps (match Python dashboard exactly) ---

export const WEATHER_ICONS: Record<number, string> = {
  0: '☀️', 1: '⛅', 2: '☁️',
  3: '🌦️', 4: '🌧️', 5: '⛈️',
};

export const WEATHER_NAMES: Record<number, string> = {
  0: 'Clear', 1: 'Light Cloud', 2: 'Overcast',
  3: 'Light Rain', 4: 'Heavy Rain', 5: 'Storm',
};

export const FUEL_MIX_NAMES: Record<number, string> = {
  0: 'Lean', 1: 'Standard', 2: 'Rich', 3: 'Max',
};

export const ERS_MODE_NAMES: Record<number, string> = {
  0: 'None', 1: 'Medium', 2: 'Hotlap', 3: 'Overtake',
};

export const COMPOUND_NAMES: Record<number, string> = {
  16: 'S', 17: 'M', 18: 'H', 7: 'I', 8: 'W',
};

export const COMPOUND_COLORS: Record<number, string> = {
  16: '#f85149', 17: '#d29922', 18: '#ffffff', 7: '#2ea043', 8: '#58a6ff',
};

export const SAFETY_CAR_NAMES: Record<number, string> = {
  0: '', 1: 'SAFETY CAR', 2: 'VIRTUAL SC', 3: 'FORMATION LAP',
};

export const SESSION_TYPE_NAMES: Record<number, string> = {
  0: 'Unknown', 1: 'P1', 2: 'P2', 3: 'P3', 4: 'Short P',
  5: 'Q1', 6: 'Q2', 7: 'Q3', 8: 'Short Q', 9: 'OSQ',
  10: 'Race', 11: 'Race 2', 12: 'Race 3',
  13: 'Time Trial',
};

export const EVENT_NAMES: Record<string, string> = {
  SSTA: 'Session Started', SEND: 'Session Ended',
  FTLP: 'Fastest Lap', RTMT: 'Retirement',
  DRSE: 'DRS Enabled', DRSD: 'DRS Disabled',
  TMPT: 'Team Mate In Pits', CHQF: 'Chequered Flag',
  RCWN: 'Race Winner', PENA: 'Penalty Issued',
  SPTP: 'Speed Trap', STLG: 'Start Lights',
  LGOT: 'Lights Out', DTSV: 'Drive Through Served',
  SGSV: 'Stop Go Served', FLBK: 'Flashback',
  BUTN: 'Button', OVTK: 'Overtake',
};

// --- F1 25 Button Bitmask (from BUTN event ButtonStatus uint32) ---
// Each bit represents a button. Multiple buttons can be pressed simultaneously.
export const BUTTON_FLAGS: Record<number, string> = {
  0x00000001: 'Cross / A',
  0x00000002: 'Triangle / Y',
  0x00000004: 'Circle / B',
  0x00000008: 'Square / X',
  0x00000010: 'D-pad Left',
  0x00000020: 'D-pad Right',
  0x00000040: 'D-pad Up',
  0x00000080: 'D-pad Down',
  0x00000100: 'Options / Menu',
  0x00000200: 'L1 / LB',
  0x00000400: 'R1 / RB',
  0x00000800: 'L2 / LT',
  0x00001000: 'R2 / RT',
  0x00002000: 'Left Stick',
  0x00004000: 'Right Stick',
  0x00008000: 'Right Stick Left',
  0x00010000: 'Right Stick Right',
  0x00020000: 'Right Stick Up',
  0x00040000: 'Right Stick Down',
  0x00080000: 'Special',
  0x00100000: 'UDP Action 1',
  0x00200000: 'UDP Action 2',
  0x00400000: 'UDP Action 3',
  0x00800000: 'UDP Action 4',
  0x01000000: 'UDP Action 5',
  0x02000000: 'UDP Action 6',
  0x04000000: 'UDP Action 7',
  0x08000000: 'UDP Action 8',
  0x10000000: 'UDP Action 9',
  0x20000000: 'UDP Action 10',
  0x40000000: 'UDP Action 11',
  0x80000000: 'UDP Action 12',
};

/** Decode a ButtonStatus bitmask into a list of pressed button names. */
export function decodeButtons(status: number): string[] {
  const pressed: string[] = [];
  for (const [mask, name] of Object.entries(BUTTON_FLAGS)) {
    if (status & Number(mask)) {
      pressed.push(name);
    }
  }
  return pressed;
}

// --- Color thresholds ---

export const TIRE_WEAR_THRESHOLDS = { good: 25, warn: 45 } as const;
export const SURFACE_TEMP_THRESHOLDS = { cold: 80, hot: 110 } as const;
export const INNER_TEMP_THRESHOLDS = { cold: 85, hot: 105 } as const;
export const BRAKE_TEMP_THRESHOLDS = { cold: 200, hot: 900 } as const;
export const DAMAGE_THRESHOLDS = { good: 25, warn: 50 } as const;

// --- ERS / Fuel scaling ---
export const ERS_MAX_JOULES = 4_000_000;
export const FUEL_TANK_MAX = 110;
export const RPM_MAX = 13_000;

// --- Tire position labels ---
export const TIRE_LABELS = ['FL', 'FR', 'RL', 'RR'] as const;
