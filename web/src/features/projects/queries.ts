import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { client, unwrap } from '../../api/client';
import { keys } from '../../api/keys';
import { useOrgSlug } from '../../api/org';
import type { CreateProjectInput, Project, UpdateProjectInput } from '../../api/types';
import type { components } from '../../api/schema';

export type Milestone = components['schemas']['Milestone'];
export type ProjectUpdate = components['schemas']['ProjectUpdate'];
export type ProjectUpdateHealth = components['schemas']['ProjectUpdateHealth'];

const MAX_PAGES = 20;

/** Every project of the org that is not archived (the list page and the sidebar need no other call). */
export function useAllProjects(enabled = true) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.projectsAll(org),
    enabled,
    queryFn: async () => {
      const items: Project[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < MAX_PAGES; i++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/projects', {
            params: { path: { org }, query: { limit: 50, cursor } },
          }),
        );
        items.push(...r.items);
        if (!r.nextCursor) break;
        cursor = r.nextCursor;
      }
      return items;
    },
    staleTime: 30_000,
  });
}

export function useMilestones(projectId: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.milestones(org, projectId),
    queryFn: async () => {
      const items: Milestone[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < MAX_PAGES; i++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/milestones', {
            params: { path: { org }, query: { project: projectId, limit: 50, cursor } },
          }),
        );
        items.push(...r.items);
        if (!r.nextCursor) break;
        cursor = r.nextCursor;
      }
      return items;
    },
    staleTime: 30_000,
  });
}

/** Add a milestone to a project (LAUN-177: the New project dialog makes them right after the project). */
export function useCreateMilestone() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (body: components['schemas']['CreateMilestoneInput']) =>
      unwrap(client.POST('/api/v1/orgs/{org}/milestones', { params: { path: { org } }, body })),
    onSuccess: (_m, body) => void qc.invalidateQueries({ queryKey: keys.milestones(org, body.projectId) }),
  });
}

/** A project's status updates, newest first (LAUN-196). */
export function useProjectUpdates(projectId: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.projectUpdates(org, projectId),
    queryFn: async () => {
      const items: ProjectUpdate[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < MAX_PAGES; i++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/projects/{project}/updates', {
            params: { path: { org, project: projectId }, query: { limit: 50, cursor } },
          }),
        );
        items.push(...r.items);
        if (!r.nextCursor) break;
        cursor = r.nextCursor;
      }
      return items;
    },
    staleTime: 30_000,
  });
}

/** Post an update. The body is sent as typed. One Idempotency-Key per attempt: an update is permanent. */
export function useCreateProjectUpdate(projectId: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ body, key }: { body: components['schemas']['CreateProjectUpdateInput']; key: string }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/projects/{project}/updates', {
          params: { path: { org, project: projectId }, header: { 'Idempotency-Key': key } },
          body,
        }),
      ),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.projectUpdates(org, projectId) }),
  });
}

type AvatarType = 'image/png' | 'image/jpeg' | 'image/gif' | 'image/webp';

/** Set or replace a project's logo (LAUN-162): prepare, PUT the bytes to the store, confirm. */
export function useSetProjectAvatar(projectId: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: async (file: File) => {
      const prep = await unwrap(
        client.POST('/api/v1/orgs/{org}/projects/{project}/avatar/prepare', {
          params: { path: { org, project: projectId } },
          body: { contentType: file.type as AvatarType, sizeBytes: file.size },
        }),
      );
      const put = await fetch(prep.uploadUrl, { method: 'PUT', headers: prep.headers, body: file });
      if (!put.ok) throw new Error('upload failed');
      return unwrap(
        client.PUT('/api/v1/orgs/{org}/projects/{project}/avatar', {
          params: { path: { org, project: projectId } },
          body: { uploadKey: prep.uploadKey },
        }),
      );
    },
    onSuccess: (saved: Project) => afterProjectAvatar(qc, org, saved),
  });
}

/** Remove a project's logo. */
export function useClearProjectAvatar(projectId: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: () =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/projects/{project}/avatar', {
          params: { path: { org, project: projectId } },
        }),
      ),
    onSuccess: (saved: Project) => afterProjectAvatar(qc, org, saved),
  });
}

/** The new `avatarUrl` (and its `?v`) must show in the detail, the list, the pickers and the issue chips. */
function afterProjectAvatar(qc: QueryClient, org: string, saved: Project) {
  qc.setQueryData(keys.project(org, saved.id), saved);
  void qc.invalidateQueries({ queryKey: keys.projectsAll(org) });
  void qc.invalidateQueries({ queryKey: ['org', org, 'projects'] });
}

/** Every list that shows a project: the all-projects list, the per-team lists and the detail. */
function refreshProjects(qc: QueryClient, org: string) {
  void qc.invalidateQueries({ queryKey: keys.projectsAll(org) });
  void qc.invalidateQueries({ queryKey: ['org', org, 'projects'] });
}

/** Delete a project (LAUN-191). Its issues stay and lose the project, so the issue lists refresh too. */
export function useDeleteProject() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (projectId: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/projects/{project}', {
          params: { path: { org, project: projectId } },
        }),
      ),
    // Only a success drops the cached detail: a 403 or 404 must keep what is on screen.
    onSuccess: (_d, projectId) => qc.removeQueries({ queryKey: keys.project(org, projectId) }),
    onSettled: () => {
      refreshProjects(qc, org);
      void qc.invalidateQueries({ queryKey: keys.issues(org) });
      void qc.invalidateQueries({ queryKey: keys.issueDetails(org) });
    },
  });
}

/** Create a project (LAUN-166). One Idempotency-Key per dialog, so a retry cannot create twice. */
export function useCreateProject() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ input, idempotencyKey }: { input: CreateProjectInput; idempotencyKey: string }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/projects', {
          params: { path: { org }, header: { 'Idempotency-Key': idempotencyKey } },
          body: input,
        }),
      ),
    onSuccess: (saved: Project) => {
      qc.setQueryData(keys.project(org, saved.id), saved);
      refreshProjects(qc, org);
    },
  });
}

/** Edit a project (LAUN-166): PATCH with the `version` the person saw. A 409 also refreshes, so a retry sees the new one. */
export function useUpdateProject(projectId: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (body: UpdateProjectInput) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/projects/{project}', {
          params: { path: { org, project: projectId } },
          body,
        }),
      ),
    onSuccess: (saved: Project) => qc.setQueryData(keys.project(org, saved.id), saved),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: keys.project(org, projectId) });
      refreshProjects(qc, org);
    },
  });
}
