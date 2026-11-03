import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { client, unwrap } from './client';
import { keys } from './keys';
import { useOrgSlug } from './org';
import type { ProjectStatus } from './types';

// LAUN-195 (decisions-T29): the org's project statuses. Six fixed kinds, any number of statuses of each.

/** The kinds in the order a project moves through them: pickers, filters and the settings page list by it. */
export const KIND_SEQUENCE: ProjectStatus['kind'][] = [
  'backlog',
  'planned',
  'started',
  'paused',
  'completed',
  'canceled',
];

/** Kind order first, then the org's order inside a kind (the server sorts the same way; this keeps a cache honest). */
export function sortStatuses(list: ProjectStatus[]): ProjectStatus[] {
  return [...list].sort(
    (a, b) => KIND_SEQUENCE.indexOf(a.kind) - KIND_SEQUENCE.indexOf(b.kind) || a.position - b.position,
  );
}

const selectStatuses = (r: { items: ProjectStatus[] }): ProjectStatus[] => sortStatuses(r.items);

/** The whole ordered list (at most 60). Any member may read it; the pickers and the filter use it. */
export function useProjectStatuses() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.projectStatuses(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/project-statuses', { params: { path: { org } } })),
    select: selectStatuses,
    staleTime: 30_000,
  });
}

/** A status name, colour or order shows on every project, so every project list refreshes with the statuses. */
function refresh(qc: QueryClient, org: string): Promise<unknown> {
  return Promise.all([
    qc.invalidateQueries({ queryKey: keys.projectStatuses(org) }),
    qc.invalidateQueries({ queryKey: keys.projectsAll(org) }),
    qc.invalidateQueries({ queryKey: ['org', org, 'projects'] }),
    qc.invalidateQueries({ queryKey: keys.projectDetails(org) }),
  ]);
}

export function useCreateProjectStatus() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (body: { kind: ProjectStatus['kind']; name: string; color: string }) =>
      unwrap(client.POST('/api/v1/orgs/{org}/project-statuses', { params: { path: { org } }, body })),
    onSettled: () => refresh(qc, org),
  });
}

export function useUpdateProjectStatus() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: { name?: string; color?: string } }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/project-statuses/{status}', {
          params: { path: { org, status: id } },
          body,
        }),
      ),
    onSettled: () => refresh(qc, org),
  });
}

export function useDeleteProjectStatus() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/project-statuses/{status}', {
          params: { path: { org, status: id } },
        }),
      ),
    onSettled: () => refresh(qc, org),
  });
}

/** PUT the whole order: every status id once. */
export function useReorderProjectStatuses() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (ids: string[]) =>
      unwrap(
        client.PUT('/api/v1/orgs/{org}/project-statuses/order', { params: { path: { org } }, body: { ids } }),
      ),
    onSettled: () => refresh(qc, org),
  });
}
