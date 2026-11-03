import type { QueryClient } from '@tanstack/react-query';

/**
 * decisions-T20 #76 (LAUN-228): a count per query client and org of the times the data of lost teams was dropped.
 * An optimistic write that takes a snapshot reads it first, and on a failure restores the snapshot only when no drop
 * happened since: else it would put rows of a team that is now hidden back into the cache.
 */
const gens = new WeakMap<QueryClient, Map<string, number>>();

export function accessGeneration(qc: QueryClient, org: string): number {
  return gens.get(qc)?.get(org) ?? 0;
}

export function bumpAccessGeneration(qc: QueryClient, org: string): void {
  let m = gens.get(qc);
  if (!m) gens.set(qc, (m = new Map()));
  m.set(org, (m.get(org) ?? 0) + 1);
}

/** Put snapshot entries back, unless the data of a lost team was dropped after `gen` was read. */
export function restoreSnapshot(
  qc: QueryClient,
  org: string,
  gen: number,
  entries: Iterable<readonly [readonly unknown[], unknown]>,
): boolean {
  if (accessGeneration(qc, org) !== gen) return false;
  for (const [k, d] of entries) qc.setQueryData(k, d);
  return true;
}
