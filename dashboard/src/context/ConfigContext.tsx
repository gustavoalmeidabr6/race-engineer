import { createContext, useContext, type ReactNode } from 'react';
import { useConfigInternal, type UseConfigResult } from '../hooks/useConfig';

/**
 * ConfigContext shares a SINGLE useConfig instance across the tree so the
 * top-level FirstLaunchGate, the RequiredSettingsBanner, and the Settings
 * page all see the same schema/values/missingRequired bundle.
 *
 * Without this, each call to useConfig() built its own state. Saving a
 * Required key from Settings refreshed only Settings' local copy; the
 * banner and the launch gate kept their stale "missing required" view
 * and the next navigation re-redirected the user back to /settings.
 */
const ConfigContext = createContext<UseConfigResult | null>(null);

export function ConfigProvider({ children }: { children: ReactNode }) {
  const value = useConfigInternal();
  return <ConfigContext.Provider value={value}>{children}</ConfigContext.Provider>;
}

export function useConfig(): UseConfigResult {
  const ctx = useContext(ConfigContext);
  if (!ctx) {
    throw new Error('useConfig must be used inside <ConfigProvider>');
  }
  return ctx;
}
