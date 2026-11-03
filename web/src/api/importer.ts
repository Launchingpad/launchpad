import { useRef } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError, client, unwrap } from './client';
import { keys } from './keys';
import { useOrgSlug } from './org';
import type { ImportJob, ImportSession, ImportUserMap } from './types';

// LAUN-10: the Linear import wizard. Rules: decisions-T17 #36 to #52, web-decisions "Linear import".

export const JOB_POLL_MS = 2000;
export const JOB_POLL_MAX_MS = 30_000;

export function activeJob(s: ImportSession | null | undefined): ImportJob | null {
  if (!s) return null;
  const all = [s.jobs.export, s.jobs.dryRun, s.jobs.import, s.jobs.verify];
  return all.find((j) => j && (j.state === 'queued' || j.state === 'running')) ?? null;
}

/** True while something runs, so the page must keep asking. */
export function isBusy(s: ImportSession | null | undefined): boolean {
  if (!s) return false;
  return activeJob(s) !== null || s.state === 'exporting' || s.state === 'importing';
}

/** The wait before the next poll: 2 s, doubled after each failed poll, at most 30 s. */
export function pollDelay(failures: number): number {
  return Math.min(JOB_POLL_MS * 2 ** Math.max(0, failures), JOB_POLL_MAX_MS);
}

/**
 * The open session of the org, or null when there is none (the route answers 404). While a job runs it
 * polls (the session holds the last job of each kind with its progress); it stops when nothing runs, and
 * the observer's unmount stops it. A failed poll backs off. Nothing here holds the Linear key.
 */
export function useImportSession() {
  const org = useOrgSlug();
  // Failed polls in a row. A success sets it back to 0, so an old error does not slow later polls.
  const failures = useRef(0);
  return useQuery({
    queryKey: keys.importSession(org),
    queryFn: async (): Promise<ImportSession | null> => {
      try {
        const s = await unwrap(
          client.GET('/api/v1/orgs/{org}/import/sessions/current', { params: { path: { org } } }),
        );
        failures.current = 0;
        return s;
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          failures.current = 0;
          return null;
        }
        failures.current++;
        throw e;
      }
    },
    retry: false,
    staleTime: 0,
    refetchInterval: (q) => {
      if (q.state.status === 'error' && !isBusy(q.state.data)) return false;
      return isBusy(q.state.data) ? pollDelay(failures.current) : false;
    },
  });
}

export function useImportUsers(sessionId: string | undefined) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.importUsers(org, sessionId ?? ''),
    enabled: !!sessionId,
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/import/sessions/{session}/users', {
          params: { path: { org, session: sessionId! } },
        }),
      ),
    retry: false,
    staleTime: 0,
    refetchOnWindowFocus: false,
  });
}

/**
 * Every write of the wizard. They are plain async functions, NOT useMutation: the export body holds the
 * Linear API key, and a mutation keeps its variables in the mutation cache. The key goes straight into the
 * request and nowhere else. The export request sends no Idempotency-Key (the server refuses one).
 */
export function useImportActions() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const refresh = () => qc.invalidateQueries({ queryKey: keys.importSession(org) });
  const path = (session: string) => ({ params: { path: { org, session } } });

  return {
    async start(): Promise<ImportSession> {
      const s = await unwrap(
        client.POST('/api/v1/orgs/{org}/import/sessions', { params: { path: { org } } }),
      );
      await refresh();
      return s;
    },
    async startExport(session: string, linearApiKey: string): Promise<void> {
      try {
        await unwrap(
          client.POST('/api/v1/orgs/{org}/import/sessions/{session}/export', {
            ...path(session),
            body: { linearApiKey },
          }),
        );
      } finally {
        await refresh();
      }
    },
    async saveMap(session: string, map: ImportUserMap): Promise<void> {
      await unwrap(
        client.PUT('/api/v1/orgs/{org}/import/sessions/{session}/usermap', { ...path(session), body: map }),
      );
      await refresh();
    },
    async dryRun(session: string, acceptFailedFiles: boolean): Promise<void> {
      try {
        await unwrap(
          client.POST('/api/v1/orgs/{org}/import/sessions/{session}/dry-run', {
            ...path(session),
            body: acceptFailedFiles ? { acceptFailedFiles: true } : {},
          }),
        );
      } finally {
        await refresh();
      }
    },
    async run(session: string, confirmOrg: string): Promise<void> {
      try {
        await unwrap(
          client.POST('/api/v1/orgs/{org}/import/sessions/{session}/run', {
            ...path(session),
            body: { confirmOrg },
          }),
        );
      } finally {
        await refresh();
      }
    },
    async verify(session: string): Promise<void> {
      try {
        await unwrap(client.POST('/api/v1/orgs/{org}/import/sessions/{session}/verify', path(session)));
      } finally {
        await refresh();
      }
    },
    async cancel(job: string): Promise<void> {
      try {
        await unwrap(
          client.POST('/api/v1/orgs/{org}/import/jobs/{job}/cancel', { params: { path: { org, job } } }),
        );
      } finally {
        await refresh();
      }
    },
    async close(session: string): Promise<void> {
      try {
        await unwrap(client.DELETE('/api/v1/orgs/{org}/import/sessions/{session}', path(session)));
      } finally {
        await refresh();
      }
    },
  };
}
