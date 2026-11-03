import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { client, unwrap } from './client';
import { keys } from './keys';
import { useOrgSlug } from './org';
import { refreshProjectCounts } from './events';
import type { components } from './schema';

// LAUN-194: project labels (decisions-T28). Org-wide, with one level of groups. Not the issue labels.

type S = components['schemas'];
export type ProjectLabelInput = S['ProjectLabelInput'];
export type UpdateProjectLabelInput = S['UpdateProjectLabelInput'];

/** The whole list (no paging), by name, each with `projectCount`. */
export function useProjectLabels(enabled = true) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.projectLabels(org),
    enabled,
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/project-labels', { params: { path: { org } } })),
    select: (r) => r.items,
    staleTime: 60_000,
  });
}

/** Also on error: the list must show what the server holds. A rename or delete changes what projects show. */
function refresh(qc: QueryClient, org: string): Promise<unknown> {
  refreshProjectCounts(qc, org);
  return qc.invalidateQueries({ queryKey: keys.projectLabels(org) });
}

export function useCreateProjectLabel() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (body: ProjectLabelInput) =>
      unwrap(client.POST('/api/v1/orgs/{org}/project-labels', { params: { path: { org } }, body })),
    onSettled: () => refresh(qc, org),
  });
}

export function useUpdateProjectLabel() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: UpdateProjectLabelInput }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/project-labels/{label}', {
          params: { path: { org, label: id } },
          body,
        }),
      ),
    onSettled: () => refresh(qc, org),
  });
}

export function useDeleteProjectLabel() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/project-labels/{label}', {
          params: { path: { org, label: id } },
        }),
      ),
    onSettled: () => refresh(qc, org),
  });
}
