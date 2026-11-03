import type { ReactNode } from 'react';
import ui from '../styles/ui.module.css';

/**
 * One column for alerts and banners that can show together (LAUN-150). Every child gets its own row,
 * with a gap and no overlap, and a long word wraps instead of pushing the row wider. It adds no role:
 * each alert keeps its own. An empty stack takes no space.
 */
export function AlertStack({ children }: { children: ReactNode }) {
  return (
    <div className={ui.alertStack} data-testid="alert-stack">
      {children}
    </div>
  );
}
