import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import type { Issue } from '../../api/types';

export interface CreateDefaults {
  team?: string;
  /** Team keys the Team picker lists first (the product page, LAUN-65). */
  firstTeams?: string[];
  parent?: string;
  state?: string;
  assignee?: string;
}
interface UiState {
  create: CreateDefaults | null;
  openCreate: (d?: CreateDefaults) => void;
  closeCreate: () => void;
  paletteOpen: boolean;
  setPaletteOpen: (v: boolean) => void;
  helpOpen: boolean;
  setHelpOpen: (v: boolean) => void;
  sidebarOpen: boolean;
  setSidebarOpen: (v: boolean) => void;
  /** LAUN-236: the New issue dialog made an issue. The board's and the list's undo listens (useIssueUndo.tsx). */
  issueCreated: (issue: Issue) => void;
  /** Listen to `issueCreated`; returns the function that stops listening. */
  onIssueCreated: (fn: (issue: Issue) => void) => () => void;
}
const Ctx = createContext<UiState | null>(null);

export function UiProvider({ children }: { children: ReactNode }) {
  const [create, setCreate] = useState<CreateDefaults | null>(null);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const openCreate = useCallback((d: CreateDefaults = {}) => setCreate(d), []);
  const closeCreate = useCallback(() => setCreate(null), []);
  const listeners = useRef(new Set<(issue: Issue) => void>());
  const issueCreated = useCallback((issue: Issue) => {
    for (const fn of [...listeners.current]) fn(issue);
  }, []);
  const onIssueCreated = useCallback((fn: (issue: Issue) => void) => {
    listeners.current.add(fn);
    return () => {
      listeners.current.delete(fn);
    };
  }, []);
  const value = useMemo(
    () => ({
      create,
      openCreate,
      closeCreate,
      paletteOpen,
      setPaletteOpen,
      helpOpen,
      setHelpOpen,
      sidebarOpen,
      setSidebarOpen,
      issueCreated,
      onIssueCreated,
    }),
    [create, openCreate, closeCreate, paletteOpen, helpOpen, sidebarOpen, issueCreated, onIssueCreated],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useUi(): UiState {
  const v = useContext(Ctx);
  if (!v) throw new Error('useUi outside UiProvider');
  return v;
}
