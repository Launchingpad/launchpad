import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type QueryClient,
} from '@tanstack/react-query';
import { client, unwrap } from './client';
import { keys } from './keys';
import { useOrgSlug } from './org';
import type {
  BackfillResult,
  CreateGitIntegrationInput,
  CreateGitRepoInput,
  GitIntegration,
  GitIntegrationCheck,
  GitIntegrationCreated,
  GitRepo,
  GitRotateResult,
  HookRefreshResult,
  UpdateGitIntegrationInput,
} from './types';

// LAUN-9: git connections (decisions-T17 #46, #47, #51).
//
// Secrets. A token and a webhook secret travel in three calls: create, PATCH with a token, and rotate. Each is a
// PLAIN ASYNC function here, not a mutation: a mutation keeps its variables and its result (the secret) in the
// client's mutation cache. These functions never write a response into the query cache; they only mark the
// lists stale. The caller keeps the secret in component state and clears it. Nothing else here holds a secret.

/**
 * Marks the list, the integration and its mapped repos stale. The available-repos pages are left alone on
 * purpose: a refetch is a host call, and those calls are rate limited.
 */
function refreshGit(qc: QueryClient, org: string, id?: string) {
  void qc.invalidateQueries({ queryKey: keys.gitIntegrations(org), exact: true });
  if (id) {
    void qc.invalidateQueries({ queryKey: keys.gitIntegration(org, id), exact: true });
    void qc.invalidateQueries({ queryKey: keys.gitRepos(org, id), exact: true });
  }
}

export function useGitIntegrations() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitIntegrations(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/integrations', { params: { path: { org } } })),
    select: (r) => r.items,
    staleTime: 15_000,
  });
}

export function useGitIntegration(id: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitIntegration(org, id),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/integrations/{integration}', {
          params: { path: { org, integration: id } },
        }),
      ),
    staleTime: 15_000,
  });
}

export function useGitRepos(id: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitRepos(org, id),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/integrations/{integration}/repos', {
          params: { path: { org, integration: id } },
        }),
      ).then((r) => r.items),
    staleTime: 15_000,
  });
}

/** The deliveries ring of one integration, newest first (LAUN-87, at most 100, no paging). A summary only. */
export function useGitDeliveries(id: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitDeliveries(org, id),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/integrations/{integration}/deliveries', {
          params: { path: { org, integration: id } },
        }),
      ).then((r) => r.items),
    staleTime: 15_000,
  });
}

/**
 * The provider's repositories, a page at a time (token mode). The host call is rate limited (6 a minute), so
 * nothing refetches by itself: the page asks by hand with `enabled`, and a new search is a new key.
 */
export function useAvailableRepos(id: string, q: string, enabled: boolean) {
  const org = useOrgSlug();
  const qc = useQueryClient();
  const queryKey = keys.gitAvailable(org, id, q);
  // A key whose query errored is not fetched again just because the search text came back to it: each fetch spends
  // one of the 6 host calls a minute. `retryOnMount: false` is not enough here (it covers a mount; a key change on a
  // live observer refetches any query with no data), so the query is switched off. The page's own press on Search
  // calls `refetch()`, which works on a disabled query.
  const failed = qc.getQueryState(queryKey)?.status === 'error';
  return useInfiniteQuery({
    queryKey,
    enabled: enabled && !failed,
    retry: false,
    retryOnMount: false,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/integrations/{integration}/available-repos', {
          params: {
            path: { org, integration: id },
            query: { ...(q ? { q } : {}), ...(pageParam ? { cursor: pageParam } : {}) },
          },
        }),
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
}

/** Makes an integration. The 201 may hold `webhookSecret`, once: the caller keeps it in state only. */
export function useCreateGitIntegration() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return async (input: CreateGitIntegrationInput): Promise<GitIntegrationCreated> => {
    const created = await unwrap(
      client.POST('/api/v1/orgs/{org}/integrations', { params: { path: { org } }, body: input }),
    );
    refreshGit(qc, org);
    return created;
  };
}

/** A PATCH that carries a token (replace it, or add one and switch to token mode). The token is not kept. */
export function useReplaceGitToken() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return async (id: string, token: string, mode?: 'token'): Promise<GitIntegration> => {
    const body: UpdateGitIntegrationInput = { token, ...(mode ? { mode } : {}) };
    const updated = await unwrap(
      client.PATCH('/api/v1/orgs/{org}/integrations/{integration}', {
        params: { path: { org, integration: id } },
        body,
      }),
    );
    refreshGit(qc, org, id);
    return updated;
  };
}

/** Removes a connection (LAUN-104). 409 repos_mapped while a repository is mapped. */
export function useDeleteGitIntegration() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return async (id: string): Promise<void> => {
    await unwrap(
      client.DELETE('/api/v1/orgs/{org}/integrations/{integration}', {
        params: { path: { org, integration: id } },
      }),
    );
    qc.removeQueries({ queryKey: keys.gitIntegration(org, id), exact: true });
    qc.removeQueries({ queryKey: keys.gitRepos(org, id), exact: true });
    void qc.invalidateQueries({ queryKey: keys.gitIntegrations(org), exact: true });
  };
}

/** Starts or repeats a rotation. In webhook-only mode the answer holds the new secret, once. */
export function useRotateGitSecret() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return async (id: string): Promise<GitRotateResult> => {
    try {
      return await unwrap(
        client.POST('/api/v1/orgs/{org}/integrations/{integration}/rotate-secret', {
          params: { path: { org, integration: id } },
          body: {},
        }),
      );
    } finally {
      // Even a failed push changes the repos' hook state.
      refreshGit(qc, org, id);
    }
  };
}

/** Calls that carry no secret in either direction. */
export function useGitActions(id: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const path = { org, integration: id };
  const refresh = () => refreshGit(qc, org, id);
  return {
    setEnabled: useMutation({
      mutationFn: (enabled: boolean) =>
        unwrap(
          client.PATCH('/api/v1/orgs/{org}/integrations/{integration}', {
            params: { path },
            body: { enabled },
          }),
        ),
      onSettled: refresh,
    }),
    check: useMutation({
      mutationFn: (): Promise<GitIntegrationCheck> =>
        unwrap(client.POST('/api/v1/orgs/{org}/integrations/{integration}/check', { params: { path } })),
      onSettled: refresh,
    }),
    commit: useMutation({
      mutationFn: (force: boolean) =>
        unwrap(
          client.POST('/api/v1/orgs/{org}/integrations/{integration}/rotate-secret/commit', {
            params: { path },
            body: { force },
          }),
        ),
      onSettled: refresh,
    }),
    discard: useMutation({
      mutationFn: () =>
        unwrap(
          client.DELETE('/api/v1/orgs/{org}/integrations/{integration}/rotate-secret', {
            params: { path },
          }),
        ),
      onSettled: refresh,
    }),
    mapRepo: useMutation({
      mutationFn: (body: CreateGitRepoInput): Promise<GitRepo> =>
        unwrap(
          client.POST('/api/v1/orgs/{org}/integrations/{integration}/repos', {
            params: { path },
            body,
          }),
        ),
      onSettled: refresh,
    }),
    setTeam: useMutation({
      mutationFn: (v: { repo: string; teamId: string | null }) =>
        unwrap(
          client.PATCH('/api/v1/orgs/{org}/integrations/{integration}/repos/{repo}', {
            params: { path: { ...path, repo: v.repo } },
            body: { teamId: v.teamId },
          }),
        ),
      onSettled: refresh,
    }),
    // LAUN-95 (LAUN-83 server). Token mode only. The answers hold no secret. A hook refresh changes hookState.
    refreshHook: useMutation({
      mutationFn: (repo: string): Promise<HookRefreshResult> =>
        unwrap(
          client.POST('/api/v1/orgs/{org}/integrations/{integration}/repos/{repo}/hook/refresh', {
            params: { path: { ...path, repo } },
          }),
        ),
      onSettled: refresh,
    }),
    // A backfill changes tickets, so the issue pages are stale too.
    backfill: useMutation({
      mutationFn: (repo: string): Promise<BackfillResult> =>
        unwrap(
          client.POST('/api/v1/orgs/{org}/integrations/{integration}/repos/{repo}/backfill', {
            params: { path: { ...path, repo } },
          }),
        ),
      onSettled: () => {
        refresh();
        void qc.invalidateQueries({ queryKey: keys.issues(org) });
        void qc.invalidateQueries({ queryKey: keys.issueDetails(org) });
      },
    }),
    unmap: useMutation({
      mutationFn: (repo: string) =>
        unwrap(
          client.DELETE('/api/v1/orgs/{org}/integrations/{integration}/repos/{repo}', {
            params: { path: { ...path, repo } },
          }),
        ),
      onSettled: refresh,
    }),
  };
}
