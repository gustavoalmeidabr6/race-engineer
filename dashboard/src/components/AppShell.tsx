import type { ReactNode } from 'react';
import { Sidebar } from './Sidebar';

export function AppShell({ children }: { children: ReactNode }) {
  return (
    <div className="h-screen flex bg-bg text-text">
      <Sidebar />
      <main className="flex-1 min-w-0 overflow-hidden flex flex-col">{children}</main>
    </div>
  );
}
