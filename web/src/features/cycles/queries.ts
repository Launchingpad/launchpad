import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { client, unwrap } from '../../api/client';
import type { components } from '../../api/schema';
import { keys } from '../../api/keys';
import { useOrgSlug } from '../../api/org';
import type { UpdateCycleInput } from '../../api/types';

export type CycleSnapshot = components['schemas']['CycleSnapshot'];

export interface CycleProgress {
  scope: number;
  done: number;
  started: number;
}

function useSnapshotQuery<T>(
  cycleId: string,
  opts: { enabled?: boolean; final?: boolean },
  select: (rows: CycleSnapshot[]) => T,
) {
  const { enabled = true, final = false } = opts;
  const org = useOrgSlug();
  return useQuery({
    queryKey: ['org', org, 'cycle-progress', cycleId] as const,
    enabled,
    staleTime: final ? Infinity : 120_000,
    queryFn: async (): Promise<CycleSnapshot[]> => {
      const r = await unwrap(
        client.GET('/api/v1/orgs/{org}/cycles/{cycle}/snapshots', {
          params: { path: { org, cycle: cycleId } },
        }),
      );
      return r.items;
    },
    select,
  });
}

const lastProgress = (rows: CycleSnapshot[]): CycleProgress | null => {
  const last = rows[rows.length - 1];
  return last ? { scope: last.scopeCount, done: last.completedCount, started: last.startedCount } : null;
};
const allRows = (rows: CycleSnapshot[]) => rows;

/**
 * Progress of one cycle: the newest daily snapshot (the server refreshes today's row of a running cycle).
 * The call writes today's row on the server, so it is kept rare: the key is NOT under the org's
 * issues key (an issue event does not refetch it). It goes stale after 2 minutes; a final
 * (past) cycle never. `null` data: no snapshot yet. Callers skip `next` cycles (no rows).
 * The burn-up chart reads the same cache entry (`useCycleSnapshots`), so the page makes one call.
 */
export function useCycleProgress(cycleId: string, opts: { enabled?: boolean; final?: boolean } = {}) {
  return useSnapshotQuery(cycleId, opts, lastProgress);
}

/** All daily rows of a cycle, oldest day first (the burn-up chart). Same cache entry as `useCycleProgress`. */
export function useCycleSnapshots(cycleId: string, opts: { enabled?: boolean; final?: boolean } = {}) {
  return useSnapshotQuery(cycleId, opts, allRows);
}

/** Every cache a cycle write can change: the lists, the burn-up rows, and the issue lists (rollover, delete). */
function useRefreshCycles() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return () => {
    void qc.invalidateQueries({ queryKey: ['org', org, 'cycles'] });
    void qc.invalidateQueries({ queryKey: ['org', org, 'cycle-progress'] });
    void qc.invalidateQueries({ queryKey: keys.issues(org) });
    void qc.invalidateQueries({ queryKey: keys.issueDetails(org) });
  };
}

/** Edit a cycle (LAUN-172): name, description, dates. The server judges the dates (decisions-T27 #4). */
export function useUpdateCycle(cycleId: string) {
  const org = useOrgSlug();
  const refresh = useRefreshCycles();
  return useMutation({
    mutationFn: (body: UpdateCycleInput) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/cycles/{cycle}', {
          params: { path: { org, cycle: cycleId } },
          body,
        }),
      ),
    onSettled: refresh,
  });
}

/** Mark the current cycle complete now (LAUN-172). One Idempotency-Key per dialog, so a retry cannot run it twice. */
export function useCompleteCycle(cycleId: string) {
  const org = useOrgSlug();
  const refresh = useRefreshCycles();
  return useMutation({
    mutationFn: (idempotencyKey: string) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/cycles/{cycle}/complete', {
          params: { path: { org, cycle: cycleId }, header: { 'Idempotency-Key': idempotencyKey } },
        }),
      ),
    onSettled: refresh,
  });
}

/** Delete a cycle (LAUN-172). The server refuses a cycle that has issues. */
export function useDeleteCycle(cycleId: string) {
  const org = useOrgSlug();
  const refresh = useRefreshCycles();
  return useMutation({
    mutationFn: () =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/cycles/{cycle}', { params: { path: { org, cycle: cycleId } } }),
      ),
    onSettled: refresh,
  });
}
