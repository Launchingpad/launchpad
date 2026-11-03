import { act, renderHook, waitFor } from '@testing-library/react';
import { useMemo, type ReactNode } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { OrgProvider } from './org';
import { keys } from './keys';
import { useAllStates } from './queries';
import { boardColumns } from '../lib/group';
import { states, makeIssue } from '../test/mockApi';
import { testClient } from '../test/render';
import type { Team, WorkflowState } from './types';

const team = { id: 't1', key: 'ORB', name: 'Orbit' } as Team;

function setup() {
  const qc = testClient();
  qc.setQueryData(keys.teams('orbit'), [team]);
  qc.setQueryData(keys.states('orbit', 'ORB'), states);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <OrgProvider slug="orbit">{children}</OrgProvider>
    </QueryClientProvider>
  );
  // The same derivation as IssueBrowser: groups memoised on the states.
  const hook = renderHook(
    () => {
      const r = useAllStates();
      const groups = useMemo(() => boardColumns([makeIssue(1)], r.states, 'manual'), [r.states]);
      return { ...r, groups };
    },
    { wrapper },
  );
  return { qc, hook };
}

describe('useAllStates (LAUN-169)', () => {
  it('keeps states, teams and the groups built from them across re-renders with the same data', () => {
    const { hook } = setup();
    const first = hook.result.current;
    expect(first.states).toHaveLength(states.length);
    hook.rerender();
    hook.rerender();
    expect(hook.result.current.states).toBe(first.states);
    expect(hook.result.current.teams).toBe(first.teams);
    expect(hook.result.current.groups).toBe(first.groups);
  });

  it('keeps the empty arrays stable while nothing is loaded', () => {
    const qc = testClient();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>
        <OrgProvider slug="orbit">{children}</OrgProvider>
      </QueryClientProvider>
    );
    const { result, rerender } = renderHook(() => useAllStates(), { wrapper });
    const first = result.current;
    rerender();
    expect(result.current.states).toBe(first.states);
    expect(result.current.teams).toBe(first.teams);
  });

  it('makes new states and groups when the data changes', async () => {
    const { qc, hook } = setup();
    const first = hook.result.current;
    const next: WorkflowState[] = [...states, { ...states[0]!, id: 'extra', name: 'Extra' }];
    act(() => {
      qc.setQueryData(keys.states('orbit', 'ORB'), next);
    });
    await waitFor(() => expect(hook.result.current.states).toHaveLength(next.length));
    expect(hook.result.current.states).not.toBe(first.states);
    expect(hook.result.current.groups).not.toBe(first.groups);
  });

  it('makes a new states array when two teams swap their data (same team count)', async () => {
    const t2 = { id: 't2', key: 'WEB', name: 'Web' } as Team;
    const a: WorkflowState[] = [states[0]!];
    const b: WorkflowState[] = [{ ...states[1]!, id: 'w1' }];
    const qc = testClient();
    qc.setQueryData(keys.teams('orbit'), [team, t2]);
    qc.setQueryData(keys.states('orbit', 'ORB'), a);
    qc.setQueryData(keys.states('orbit', 'WEB'), b);
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>
        <OrgProvider slug="orbit">{children}</OrgProvider>
      </QueryClientProvider>
    );
    const { result } = renderHook(() => useAllStates(), { wrapper });
    const first = result.current.states;
    expect(first.map((s) => s.id)).toEqual([a[0]!.id, 'w1']);
    act(() => {
      qc.setQueryData(keys.states('orbit', 'ORB'), b);
      qc.setQueryData(keys.states('orbit', 'WEB'), a);
    });
    await waitFor(() => expect(result.current.states.map((s) => s.id)).toEqual(['w1', a[0]!.id]));
    expect(result.current.states).not.toBe(first);
  });
});
