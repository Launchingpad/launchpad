import { useRef } from 'react';
import { useInfiniteQuery, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { client, unwrap } from './client';
import { keys } from './keys';
import type {
  BackupDestination,
  BackupProbeInput,
  BackupRecipientList,
  BackupRun,
  BackupSettings,
  BackupSettingsInput,
  BackupUnsavedTestInput,
  CreateBackupDestinationInput,
  UpdateBackupDestinationInput,
} from './types';

// LAUN-78: scheduled off-site backups (platform admin only). Rules: docs/internal/decisions-T16-backups.md.
//
// Secrets. A destination secret (an access key, a password, a private key, a Dropbox app secret) and the Dropbox
// authorisation code travel in these calls: create, update, unsaved test and exchange. Each is a PLAIN ASYNC
// function, not a mutation: a mutation keeps its variables (the secret) in the client's mutation cache. These
// functions never write a request into the query cache; they only mark lists stale. The caller keeps a typed
// secret in component state and clears it. A destination read never holds a secret (`secretsSet` is names only).

/** The wait between two reads of a run. A test shortens it. */
export const pollConfig = { ms: 1500 };
export const MAX_POLL_FAILURES = 5;

const FINAL = new Set<BackupRun['status']>(['succeeded', 'failed', 'interrupted', 'canceled']);
export const isFinalRun = (r: Pick<BackupRun, 'status'>): boolean => FINAL.has(r.status);

/** Marks every backup list stale. A polled run is left alone: refreshing it from its own fetch would loop. */
function refreshAll(qc: QueryClient) {
  void qc.invalidateQueries({
    queryKey: ['platform', 'backups'],
    predicate: (q) => q.queryKey[2] !== 'run',
  });
}

export function useBackupStatus() {
  return useQuery({
    queryKey: keys.backupStatus,
    queryFn: () => unwrap(client.GET('/api/v1/platform/backups/status')),
    staleTime: 15_000,
    refetchOnWindowFocus: true,
  });
}

export function useBackupSettings() {
  return useQuery({
    queryKey: keys.backupSettings,
    queryFn: () => unwrap(client.GET('/api/v1/platform/backups/settings')),
    staleTime: 15_000,
  });
}

export function useSaveBackupSettings() {
  const qc = useQueryClient();
  return async (input: BackupSettingsInput): Promise<BackupSettings> => {
    const saved = await unwrap(client.PUT('/api/v1/platform/backups/settings', { body: input }));
    qc.setQueryData(keys.backupSettings, saved);
    refreshAll(qc);
    return saved;
  };
}

export function useBackupRecipients() {
  return useQuery({
    queryKey: keys.backupRecipients,
    queryFn: () => unwrap(client.GET('/api/v1/platform/backups/recipients')),
    staleTime: 15_000,
  });
}

export function useRecipientActions() {
  const qc = useQueryClient();
  const path = (fingerprint: string) => ({ params: { path: { fingerprint } } });
  const done = <T>(r: T): T => {
    refreshAll(qc);
    return r;
  };
  return {
    add: async (publicKey: string) =>
      done(await unwrap(client.POST('/api/v1/platform/backups/recipients', { body: { publicKey } }))),
    remove: async (fingerprint: string): Promise<BackupRecipientList> =>
      done(
        await unwrap(client.DELETE('/api/v1/platform/backups/recipients/{fingerprint}', path(fingerprint))),
      ),
    cancel: async (fingerprint: string): Promise<BackupRecipientList> =>
      done(
        await unwrap(
          client.POST('/api/v1/platform/backups/recipients/{fingerprint}/cancel', path(fingerprint)),
        ),
      ),
  };
}

export function useBackupDestinations() {
  return useQuery({
    queryKey: keys.backupDestinations,
    queryFn: () => unwrap(client.GET('/api/v1/platform/backups/destinations')),
    select: (r) => r.items,
    staleTime: 15_000,
  });
}

export function useDestinationActions() {
  const qc = useQueryClient();
  const id = (v: string) => ({ params: { path: { id: v } } });
  const done = <T>(r: T): T => {
    refreshAll(qc);
    return r;
  };
  return {
    create: async (body: CreateBackupDestinationInput): Promise<BackupDestination> =>
      done(await unwrap(client.POST('/api/v1/platform/backups/destinations', { body }))),
    update: async (dest: string, body: UpdateBackupDestinationInput): Promise<BackupDestination> =>
      done(
        await unwrap(
          client.PATCH('/api/v1/platform/backups/destinations/{id}', {
            params: { path: { id: dest } },
            body,
          }),
        ),
      ),
    remove: async (dest: string): Promise<void> => {
      await unwrap(client.DELETE('/api/v1/platform/backups/destinations/{id}', id(dest)));
      refreshAll(qc);
    },
    probeSaved: (dest: string): Promise<BackupRun> =>
      unwrap(client.POST('/api/v1/platform/backups/destinations/{id}/probe', id(dest))),
    probeUnsaved: (body: BackupProbeInput): Promise<BackupRun> =>
      unwrap(client.POST('/api/v1/platform/backups/destinations/probe', { body })),
    testSaved: (dest: string, confirmedFingerprint?: string): Promise<BackupRun> =>
      unwrap(
        client.POST('/api/v1/platform/backups/destinations/{id}/test', {
          params: { path: { id: dest } },
          body: confirmedFingerprint ? { confirmedFingerprint } : {},
        }),
      ),
    testUnsaved: (body: BackupUnsavedTestInput): Promise<BackupRun> =>
      unwrap(client.POST('/api/v1/platform/backups/destinations/test', { body })),
    exchange: (dest: string, code: string): Promise<BackupRun> =>
      unwrap(
        client.POST('/api/v1/platform/backups/destinations/{id}/exchange', {
          params: { path: { id: dest } },
          body: { code },
        }),
      ),
    runNow: async (dest: string): Promise<BackupRun> => {
      const run = await unwrap(client.POST('/api/v1/platform/backups/destinations/{id}/run', id(dest)));
      refreshAll(qc);
      return run;
    },
  };
}

/** One run, polled until it ends. A test, probe or exchange run returns its `steps`. */
export function useBackupRun(runId: string | null) {
  const qc = useQueryClient();
  const refreshed = useRef(new Set<string>());
  // Reads that failed in a row. A ref, because the interval callback must see it at once.
  const failures = useRef(0);
  const q = useQuery({
    queryKey: keys.backupRun(runId ?? ''),
    enabled: runId !== null,
    queryFn: async () => {
      let run: BackupRun;
      try {
        run = await unwrap(
          client.GET('/api/v1/platform/backups/runs/{id}', { params: { path: { id: runId ?? '' } } }),
        );
      } catch (e) {
        failures.current++;
        throw e;
      }
      failures.current = 0;
      // A finished exchange or test changes the destination (the refresh token, a saved pin): read it again.
      if (isFinalRun(run) && !refreshed.current.has(run.id)) {
        refreshed.current.add(run.id);
        refreshAll(qc);
      }
      return run;
    },
    retry: false,
    // Poll until the run ends, or until MAX_POLL_FAILURES reads failed in a row.
    refetchInterval: (query) =>
      (query.state.data && isFinalRun(query.state.data)) || failures.current >= MAX_POLL_FAILURES
        ? false
        : pollConfig.ms,
    refetchOnWindowFocus: false,
    gcTime: 0,
  });
  return { ...q, gaveUp: q.isError && failures.current >= MAX_POLL_FAILURES };
}

/** The run table, newest first, a page at a time. `destination` '' is all of them. */
export function useBackupRuns(destination: string) {
  return useInfiniteQuery({
    queryKey: keys.backupRuns(destination),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(
        client.GET('/api/v1/platform/backups/runs', {
          params: {
            query: {
              limit: 25,
              ...(pageParam ? { cursor: pageParam } : {}),
              ...(destination ? { destination } : {}),
            },
          },
        }),
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    staleTime: 15_000,
  });
}

export function useBackupChanges() {
  return useInfiniteQuery({
    queryKey: keys.backupChanges,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(
        client.GET('/api/v1/platform/backups/changes', {
          params: { query: { limit: 25, ...(pageParam ? { cursor: pageParam } : {}) } },
        }),
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    staleTime: 15_000,
  });
}
