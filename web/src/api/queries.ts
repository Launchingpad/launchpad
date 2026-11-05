import {
  useInfiniteQuery,
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type QueryClient,
} from '@tanstack/react-query';
import { useCallback, useRef } from 'react';
import { client, unwrap } from './client';
import { refreshProjectCounts } from './events';
import { SessionChangedError, getSessionEpoch } from './http';
import { ApiError } from './errors';
import { keys } from './keys';
import { useOrgSlug } from './org';
import type {
  ActivityEntry,
  UiTheme,
  UserPreferences,
  AgentInput,
  CreatedInvite,
  CreatedToken,
  Cycle,
  CreateIssueInput,
  GitWorkflowRuleInput,
  InviteInput,
  Issue,
  IssueList,
  MailTestResult,
  NotificationSettings,
  NotificationSettingsInput,
  NotificationTeamSettingsInput,
  Onboarding,
  OnboardingStepId,
  Org,
  Session,
  UpdateOrgInput,
  PlatformSettings,
  PlatformSettingsInput,
  Project,
  RelationType,
  ChatServiceCreate,
  ChatServicePatch,
  PersonalWebhookCreate,
  PersonalWebhookPatch,
  PushRelayInput,
  Team,
  TeamAccessInput,
  TokenInput,
  UpdateIssueInput,
  WorkflowState,
  Attachment,
  SignupApproveInput,
} from './types';
import { toApiQuery, type IssueFilters, type IssueApiQuery } from '../features/issues/filters';
import { accessGeneration, restoreSnapshot } from './accessGen';

const PAGE = 50;

// ---------- reference data ----------

export function useTeams() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.teams(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/teams', { params: { path: { org } } })).then((r) => r.items),
    staleTime: 60_000,
  });
}

/** The non-archived products (LAUN-64). The whole list, no paging. A failed read leaves the sidebar flat. */
export function useProducts() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.products(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/products', { params: { path: { org } } })).then((r) => r.items),
    staleTime: 60_000,
  });
}

export function useAllStates() {
  const org = useOrgSlug();
  const teams = useTeams();
  const results = useQueries({
    queries: (teams.data ?? []).map((t) => ({
      queryKey: keys.states(org, t.key),
      queryFn: () =>
        unwrap(
          client.GET('/api/v1/orgs/{org}/teams/{team}/states', { params: { path: { org, team: t.key } } }),
        ).then((r) => r.items),
      staleTime: 60_000,
    })),
  });
  // LAUN-169: `results` is a new array every render, so a plain flatMap made a new `states` each time and every
  // memo downstream (the board's groups, its column re-measure) ran again, also mid-drag. Keep the last array
  // while each team's data (structurally shared by react-query) is the same object.
  const parts = results.map((r) => r.data);
  const last = useRef<{ parts: (WorkflowState[] | undefined)[]; states: WorkflowState[] }>({
    parts: [],
    states: [],
  });
  if (last.current.parts.length !== parts.length || last.current.parts.some((p, i) => p !== parts[i])) {
    last.current = { parts, states: parts.flatMap((p) => p ?? []) };
  }
  const states = last.current.states;
  return {
    states,
    isLoading: teams.isLoading || results.some((r) => r.isLoading),
    teams: teams.data ?? NO_TEAMS,
  };
}
const NO_TEAMS: Team[] = [];

export function useTeamStates(team: string | undefined) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.states(org, team ?? ''),
    enabled: !!team,
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/teams/{team}/states', { params: { path: { org, team: team! } } }),
      ).then((r) => r.items),
    staleTime: 60_000,
  });
}

/**
 * The labels, each with the server's `issueCount`. With a team key: the org labels plus that team's own,
 * with that team's counts (decisions-T21 #5, #6). Without: every label of every team.
 */
export function useLabels(team?: string, enabled = true) {
  const org = useOrgSlug();
  return useQuery({
    // Under keys.labels(org), so one invalidation reaches every team's list.
    queryKey: [...keys.labels(org), team?.toUpperCase() ?? ''],
    enabled,
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/labels', {
          params: { path: { org }, query: team ? { team } : {} },
        }),
      ).then((r) => r.items),
    staleTime: 60_000,
  });
}

/** Cycles of one team (the server's cycle job makes them). Off until the team has cycles enabled. */
export function useCycles(team: string | undefined, enabled: boolean) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.cycles(org, team ?? ''),
    enabled: enabled && !!team,
    // Follow nextCursor to the end: an imported team can have far more than one page of cycles.
    queryFn: async () => {
      const items: Cycle[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < 40; i++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/cycles', {
            params: { path: { org }, query: { team, limit: 50, cursor } },
          }),
        );
        items.push(...r.items);
        if (!r.nextCursor) break;
        cursor = r.nextCursor;
      }
      return items;
    },
    staleTime: 60_000,
  });
}

/** Projects of one team that are not archived (the create dialog's Project chip and the issue page's Project row). */
export function useProjects(team: string | undefined) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.projects(org, team ?? ''),
    enabled: !!team,
    queryFn: async () => {
      const items: Project[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < 20; i++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/projects', {
            params: { path: { org }, query: { team, limit: 50, cursor } },
          }),
        );
        items.push(...r.items);
        if (!r.nextCursor) break;
        cursor = r.nextCursor;
      }
      return items;
    },
    staleTime: 60_000,
  });
}

/**
 * Upload files to an issue that exists (T3b): prepare, PUT the bytes to the store, attach.
 * Returns the files that failed (the same File objects); one failure does not stop the others.
 */
export function useAttachFiles() {
  const org = useOrgSlug();
  return async (
    issue: string,
    files: File[],
    hooks?: {
      onAttached?: (file: File, a: Attachment) => void;
      onFailed?: (file: File, err: unknown) => void;
    },
  ): Promise<File[]> => {
    const failed: File[] = [];
    for (const file of files) {
      try {
        const prep = await unwrap(
          client.POST('/api/v1/orgs/{org}/attachments/prepare', {
            params: { path: { org } },
            body: { filename: file.name, contentType: file.type, sizeBytes: file.size },
          }),
        );
        const put = await fetch(prep.uploadUrl, { method: 'PUT', headers: prep.headers, body: file });
        if (!put.ok) throw new Error('upload failed');
        const made = await unwrap(
          client.POST('/api/v1/orgs/{org}/issues/{issue}/attachments', {
            params: { path: { org, issue } },
            body: { kind: 'file', uploadKey: prep.uploadKey, filename: file.name },
          }),
        );
        hooks?.onAttached?.(file, made);
      } catch (err) {
        failed.push(file);
        hooks?.onFailed?.(file, err);
      }
    }
    return failed;
  };
}

export function useCreateTeam() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: { key: string; name: string }) =>
      unwrap(client.POST('/api/v1/orgs/{org}/teams', { params: { path: { org } }, body: input })),
    // The sidebar TEAMS list reads the same query, so it shows the new team with no reload.
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.teams(org) }),
  });
}

/** Rename a team (LAUN-99). The key is fixed. The sidebar, the team page and the tab title read the teams query. */
export function useRenameTeam() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ team, name }: { team: string; name: string }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/teams/{team}', { params: { path: { org, team } }, body: { name } }),
      ),
    onSuccess: (saved: Team) =>
      qc.setQueryData<Team[]>(keys.teams(org), (old) => old?.map((t) => (t.id === saved.id ? saved : t))),
    // Product team lists and issue details hold a copy of the team name.
    onSettled: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: keys.teams(org) }),
        qc.invalidateQueries({ queryKey: keys.products(org) }),
        qc.invalidateQueries({ queryKey: keys.issueDetails(org) }),
      ]),
  });
}

/** Delete a team (LAUN-191). The server refuses (409 team_in_use) while it holds issues. */
export function useDeleteTeam() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (team: string) =>
      unwrap(client.DELETE('/api/v1/orgs/{org}/teams/{team}', { params: { path: { org, team } } })),
    onSettled: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: keys.teams(org) }),
        qc.invalidateQueries({ queryKey: keys.products(org) }),
        qc.invalidateQueries({ queryKey: keys.issues(org) }),
        qc.invalidateQueries({ queryKey: keys.issueDetails(org) }),
        // The team's labels, cycles and project links go with it (LAUN-191).
        qc.invalidateQueries({ queryKey: keys.labels(org) }),
        qc.invalidateQueries({ queryKey: ['org', org, 'cycles'] }),
        qc.invalidateQueries({ queryKey: keys.projectsAll(org) }),
        qc.invalidateQueries({ queryKey: ['org', org, 'projects'] }),
        qc.invalidateQueries({ queryKey: keys.projectDetails(org) }),
      ]),
  });
}

/**
 * Turn a team's cycles on or off (LAUN-3). The switch moves at once and goes back when the server says no.
 * The sidebar's Cycles link reads the same teams query, so it follows the switch.
 */
export function useSetTeamCycles() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const key = keys.teams(org);
  return useMutation({
    mutationFn: ({ team, enabled }: { team: string; enabled: boolean }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/teams/{team}', {
          params: { path: { org, team } },
          body: { cyclesEnabled: enabled },
        }),
      ),
    onMutate: async ({ team, enabled }) => {
      await qc.cancelQueries({ queryKey: key });
      const before = qc.getQueryData<Team[]>(key);
      qc.setQueryData<Team[]>(key, (old) =>
        old?.map((t) => (t.key === team ? { ...t, cyclesEnabled: enabled } : t)),
      );
      return { before };
    },
    onError: (_e, { team }, ctx) => {
      // Put back only this team's switch: another switch may have moved since.
      const was = ctx?.before?.find((t) => t.key === team);
      if (was)
        qc.setQueryData<Team[]>(key, (old) =>
          old?.map((t) => (t.key === team ? { ...t, cyclesEnabled: was.cyclesEnabled } : t)),
        );
    },
    // The enable makes the first cycles on the server: refresh the cycle lists as well.
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: key });
      void qc.invalidateQueries({ queryKey: ['org', org, 'cycles'] });
    },
  });
}

/** One project by id: names the project of an issue when the list above does not hold it (archived). */
export function useProject(id: string | null | undefined, enabled = true) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.project(org, id ?? ''),
    enabled: !!id && enabled,
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/projects/{project}', { params: { path: { org, project: id! } } }),
      ),
    staleTime: 60_000,
  });
}

/** The active members; shared by useMembers and a fresh read after a change (the approval, LAUN-228). */
export const membersQuery = (org: string) => ({
  queryKey: keys.members(org),
  queryFn: () =>
    unwrap(client.GET('/api/v1/orgs/{org}/users', { params: { path: { org } } })).then((r) =>
      r.items.filter((m) => m.deactivatedAt === null),
    ),
  staleTime: 60_000,
});

export function useMembers() {
  const org = useOrgSlug();
  return useQuery(membersQuery(org));
}

/**
 * Set a member's team access (decisions-T20 #48, LAUN-227). `dryRun` writes nothing and answers the counts. A real
 * change refetches the members and, when it cleared assignees, the issues.
 */
export function useSetTeamAccess() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ user, body, dryRun }: { user: string; body: TeamAccessInput; dryRun: boolean }) =>
      unwrap(
        client.PUT('/api/v1/orgs/{org}/users/{user}/team-access', {
          params: { path: { org, user }, query: dryRun ? { dryRun: true } : {} },
          body,
        }),
      ),
    // A 404: the person left the org. The list is refreshed so the row goes.
    onError: (err) => {
      if (err instanceof ApiError && err.status === 404)
        void qc.invalidateQueries({ queryKey: keys.members(org) });
    },
    onSuccess: (r) => {
      if (r.dryRun) return;
      void qc.invalidateQueries({ queryKey: keys.members(org) });
      if (r.unassigned > 0) {
        void qc.invalidateQueries({ queryKey: keys.issues(org) });
        void qc.invalidateQueries({ queryKey: keys.issueDetails(org) });
      }
    },
  });
}

/** Agent users. The 201 body is the new member; the member list is refetched. */
export function useCreateAgent() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: AgentInput) =>
      unwrap(client.POST('/api/v1/orgs/{org}/agents', { params: { path: { org } }, body: input })),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.members(org) }),
  });
}

// ---------- API tokens (admin scope) ----------

/** Tokens pinned to the org. The list never holds a secret. */
export function useTokens() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.tokens(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/tokens', { params: { path: { org } } })).then((r) => r.items),
    staleTime: 30_000,
  });
}

/**
 * Make a token. This is a plain function, NOT useMutation: a mutation keeps its result in the query
 * client's mutation cache for minutes, and the result holds the secret. The caller keeps the secret in
 * component state only.
 */
export function useCreateToken() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return async (input: TokenInput): Promise<CreatedToken> => {
    const created = await unwrap(
      client.POST('/api/v1/orgs/{org}/tokens', { params: { path: { org } }, body: input }),
    );
    void qc.invalidateQueries({ queryKey: keys.tokens(org) });
    return created;
  };
}

export function useRevokeToken() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(client.DELETE('/api/v1/orgs/{org}/tokens/{token}', { params: { path: { org, token: id } } })),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.tokens(org) }),
  });
}

// ---------- my own login sessions (LAUN-160, LAUN-130) ----------

/** The caller's live login sessions. The list holds no token value. An agent token gets 403. */
export function useMySessions() {
  return useQuery({
    queryKey: keys.mySessions,
    queryFn: () => unwrap(client.GET('/api/v1/me/sessions')).then((r) => r.items),
    staleTime: 0,
    retry: false,
  });
}

/** Revoke one session. The list is refetched after the answer, success or failure (a 404 means it is gone). */
export function useRevokeMySession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(client.DELETE('/api/v1/me/sessions/{id}', { params: { path: { id } } })),
    onSettled: () => void qc.invalidateQueries({ queryKey: keys.mySessions }),
  });
}

/** Log out everywhere else. Resolves to the number revoked (it can exceed the rows shown). */
export function useRevokeOtherSessions() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => unwrap(client.POST('/api/v1/me/sessions/revoke-others')).then((r) => r.revoked),
    onSettled: () => void qc.invalidateQueries({ queryKey: keys.mySessions }),
  });
}

// ---------- my display preferences (LAUN-235, decisions-T42) ----------

/**
 * The caller's theme. Read once per page load (it changes only through this app); an agent token gets 403
 * and the app then stays on the look the browser remembered.
 */
export function useMyPreferences(enabled = true) {
  return useQuery({
    queryKey: keys.myPreferences,
    queryFn: () => unwrap(client.GET('/api/v1/me/preferences')),
    staleTime: Infinity,
    retry: false,
    enabled,
  });
}

/**
 * The theme writes of this page load (LAUN-235 review). `latest` numbers the choices in click order; `confirmed`
 * is the last value the server accepted (undefined: none yet, the server value of the GET is used).
 */
const themeWrites: { latest: number; confirmed: UiTheme | undefined } = { latest: 0, confirmed: undefined };

/** Forget the theme writes (sign-out: the next person starts clean). */
export function resetThemeWrites(): void {
  themeWrites.latest = 0;
  themeWrites.confirmed = undefined;
}

interface ThemeWrite {
  theme: UiTheme;
  seq: number;
  /** Called when THIS write is the latest and fails: the theme shown goes back to the last confirmed one. */
  onFailed?: (back: UiTheme) => void;
}

/**
 * Store the theme. Optimistic: the cached value (which ThemeSync shows) changes at once. The writes run one after
 * another (`scope`), so the server ends on the last choice. Only the answer to the LATEST choice changes what is
 * shown: an older write's success only records the confirmed value, an older write's failure is dropped (a newer
 * choice replaced it). When the latest write fails, the theme goes back to the last value the server accepted.
 */
export function useSetMyTheme() {
  const qc = useQueryClient();
  const m = useMutation({
    scope: { id: 'my-preferences' },
    mutationFn: ({ theme }: ThemeWrite) => unwrap(client.PUT('/api/v1/me/preferences', { body: { theme } })),
    onSuccess: (stored, { seq }) => {
      themeWrites.confirmed = stored.theme;
      if (seq === themeWrites.latest) qc.setQueryData<UserPreferences>(keys.myPreferences, stored);
    },
    onError: (_e, { seq, onFailed }) => {
      if (seq !== themeWrites.latest || themeWrites.confirmed === undefined) return;
      const back = themeWrites.confirmed;
      qc.setQueryData<UserPreferences>(keys.myPreferences, { theme: back });
      onFailed?.(back);
    },
  });
  const { mutate } = m;
  const choose = useCallback(
    (theme: UiTheme, shown: UiTheme, onFailed?: (back: UiTheme) => void) => {
      if (themeWrites.confirmed === undefined) {
        themeWrites.confirmed = qc.getQueryData<UserPreferences>(keys.myPreferences)?.theme ?? shown;
      }
      const seq = ++themeWrites.latest;
      void qc.cancelQueries({ queryKey: keys.myPreferences });
      qc.setQueryData<UserPreferences>(keys.myPreferences, { theme });
      mutate({ theme, seq, onFailed });
    },
    [qc, mutate],
  );
  return { choose, isPending: m.isPending };
}

// ---------- platform settings (platform admin) ----------

/** The platform settings. The body never holds a secret (only `postmarkTokenSet` and `smtpPasswordSet`). */
export function usePlatformSettings(enabled = true) {
  return useQuery({
    queryKey: keys.platform,
    enabled,
    queryFn: () => unwrap(client.GET('/api/v1/platform/settings')),
    staleTime: 60_000,
    // A refetch on focus would remount the form and drop what the person typed.
    refetchOnWindowFocus: false,
  });
}

/**
 * Save the platform settings. This is a plain function, NOT useMutation: the request body holds the
 * Postmark token and the SMTP password, and a mutation keeps its variables in the mutation cache. The
 * reply holds no secret, so it may go in the query cache.
 */
export function useSavePlatformSettings() {
  const qc = useQueryClient();
  return async (input: PlatformSettingsInput): Promise<PlatformSettings> => {
    const saved = await unwrap(client.PUT('/api/v1/platform/settings', { body: input }));
    qc.setQueryData(keys.platform, saved);
    void qc.invalidateQueries({ queryKey: keys.platform });
    return saved;
  };
}

/** Send the test email to the signed-in person. A plain function too: the page keeps the outcome in state. */
export function useTestPlatformMail() {
  return (): Promise<MailTestResult> => unwrap(client.POST('/api/v1/platform/mail/test'));
}

// ---------- connected accounts (LAUN-199, decisions-T34) ----------

/** The org's git connections with the caller's handle on each. An agent token gets 403. */
export function useGitAccounts() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitAccounts(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/me/git-accounts', { params: { path: { org } } })).then(
        (r) => r.items,
      ),
    staleTime: 0,
    retry: false,
  });
}

function useRefreshGitLinks() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  // A link changes who the Changes block shows, so the issue details are refreshed too.
  return () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: keys.gitAccounts(org) }),
      qc.invalidateQueries({ queryKey: keys.gitIdentities(org) }),
      qc.invalidateQueries({ queryKey: keys.issueDetails(org) }),
    ]);
}

/** Link or change the handle on one connection. */
export function useSaveGitAccount() {
  const org = useOrgSlug();
  const refresh = useRefreshGitLinks();
  return useMutation({
    mutationFn: (v: { integrationId: string; handle: string }) =>
      unwrap(
        client.PUT('/api/v1/orgs/{org}/me/git-accounts/{integration}', {
          params: { path: { org, integration: v.integrationId } },
          body: { handle: v.handle },
        }),
      ),
    onSuccess: () => void refresh(),
  });
}

/** Remove the caller's handle on one connection. */
export function useRemoveGitAccount() {
  const org = useOrgSlug();
  const refresh = useRefreshGitLinks();
  return useMutation({
    mutationFn: (integrationId: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/me/git-accounts/{integration}', {
          params: { path: { org, integration: integrationId } },
        }),
      ),
    onSettled: () => void refresh(),
  });
}

/** Every member's link in the org (org admin). */
export function useGitIdentities(enabled: boolean) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitIdentities(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/git-identities', { params: { path: { org } } })).then(
        (r) => r.items,
      ),
    enabled,
    staleTime: 0,
    retry: false,
  });
}

/** An admin removes one link. */
export function useRemoveGitIdentity() {
  const org = useOrgSlug();
  const refresh = useRefreshGitLinks();
  return useMutation({
    mutationFn: (identity: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/git-identities/{identity}', {
          params: { path: { org, identity } },
        }),
      ),
    onSettled: () => void refresh(),
  });
}

// ---------- agent guidance (LAUN-200, decisions-T31) ----------

/** Every guidance entry of the org: the org-wide one first, then the teams'. */
export function useAgentGuidance() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.agentGuidance(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/agent-guidance', { params: { path: { org } } })),
    staleTime: 30_000,
    refetchOnWindowFocus: false, // a refetch would not reset an editor, but there is no need
  });
}

/** Write one entry. `team` null is the org-wide text. */
export function useSaveAgentGuidance() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: { team: string | null; text: string }) =>
      unwrap(client.PUT('/api/v1/orgs/{org}/agent-guidance', { params: { path: { org } }, body: input })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.agentGuidance(org) }),
  });
}

/** Remove one entry. `team` null is the org-wide text. */
export function useClearAgentGuidance() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (team: string | null) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/agent-guidance', {
          params: { path: { org }, query: team ? { team } : {} },
        }),
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.agentGuidance(org) }),
  });
}

// ---------- git workflow rules (LAUN-244, decisions-T44) ----------

/** One team's git workflow: one rule per event, built-in or custom. */
export function useGitWorkflow(team: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.gitWorkflow(org, team),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/teams/{team}/git-workflow', { params: { path: { org, team } } })),
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });
}

/** Replace one team's custom rules. An empty list resets every event to its built-in move. */
export function useSaveGitWorkflow(team: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (rules: GitWorkflowRuleInput[]) =>
      unwrap(
        client.PUT('/api/v1/orgs/{org}/teams/{team}/git-workflow', {
          params: { path: { org, team } },
          body: { rules },
        }),
      ),
    onSuccess: (wf) => qc.setQueryData(keys.gitWorkflow(org, team), wf),
  });
}

// ---------- the org's chat services (LAUN-204, decisions-T36) ----------

/** The services as the server says them: the host, never the webhook URL (admin only). */
export function useChatServices() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.chat(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/chat-services', { params: { path: { org } } })),
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });
}

/** Add a service. The URL goes in the body once and is never read back. */
export function useCreateChatService() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: ChatServiceCreate) =>
      unwrap(client.POST('/api/v1/orgs/{org}/chat-services', { params: { path: { org } }, body: input })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.chat(org) }),
  });
}

/** Change a service. An absent field keeps its value; `webhookUrl` replaces the stored URL. */
export function useUpdateChatService() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (v: { id: string; patch: ChatServicePatch }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/chat-services/{service}', {
          params: { path: { org, service: v.id } },
          body: v.patch,
        }),
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.chat(org) }),
  });
}

/** Remove a service and its posts. */
export function useRemoveChatService() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/chat-services/{service}', {
          params: { path: { org, service: id } },
        }),
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.chat(org) }),
  });
}

/** Post the fixed test message. The outcome (lastOkAt, lastError) is on the service, so the list is read again. */
export function useTestChatService() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/chat-services/{service}/test', {
          params: { path: { org, service: id } },
        }),
      ),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.chat(org) }),
  });
}

// ---------- mobile push (LAUN-197, decisions-T35) ----------

/** The org's relay as the server says it: `configured` and the host, never the URL or the secret (admin only). */
export function usePushRelay() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.push(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/push', { params: { path: { org } } })),
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });
}

/** Set the relay URL and/or its secret. An absent field keeps the stored value. */
export function useSavePushRelay() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: PushRelayInput) =>
      unwrap(client.PUT('/api/v1/orgs/{org}/push', { params: { path: { org } }, body: input })),
    onSuccess: (data) => {
      qc.setQueryData(keys.push(org), data);
      void qc.invalidateQueries({ queryKey: keys.notificationSettings(org) });
    },
  });
}

/** Remove the relay and the pushes that wait. Registered devices stay. */
export function useRemovePushRelay() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: () => unwrap(client.DELETE('/api/v1/orgs/{org}/push', { params: { path: { org } } })),
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: keys.push(org) }),
        qc.invalidateQueries({ queryKey: keys.notificationSettings(org) }),
      ]),
  });
}

/** Send a test push to the caller's own devices. The outcome is on the relay, and a dead device is gone. */
export function useTestPush() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: () => unwrap(client.POST('/api/v1/orgs/{org}/push/test', { params: { path: { org } } })),
    onSettled: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: keys.push(org) }),
        qc.invalidateQueries({ queryKey: keys.pushDevices(org) }),
      ]),
  });
}

/** The caller's own devices. An agent token gets 403. */
export function usePushDevices() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.pushDevices(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/me/push-devices', { params: { path: { org } } })),
    staleTime: 0,
    retry: false,
  });
}

/** Remove one of the caller's devices. */
export function useRemovePushDevice() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/me/push-devices/{device}', {
          params: { path: { org, device: id } },
        }),
      ),
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: keys.pushDevices(org) }),
        // The Push choices show only while the member has a device.
        qc.invalidateQueries({ queryKey: keys.notificationSettings(org) }),
      ]),
  });
}

// ---------- onboarding state and the org's own settings (LAUN-98, server LAUN-56) ----------

/** The first-run checklist as the server works it out (org admin). Booleans and ids only, no platform setting. */
export function useOnboardingState() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.onboarding(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/onboarding', { params: { path: { org } } })),
    staleTime: 30_000,
  });
}

/** Skip a step or undo the skip, for the whole org. The reply is the new state of the checklist. */
export function useSetOnboardingSkip() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: { step: OnboardingStepId; skipped: boolean }) =>
      unwrap(client.PUT('/api/v1/orgs/{org}/onboarding/skip', { params: { path: { org } }, body: input })),
    onSuccess: (data: Onboarding) => qc.setQueryData(keys.onboarding(org), data),
  });
}

/**
 * Record the sign-up mode as chosen (a platform admin). The PUT takes the whole settings, so this reads them
 * first and sends them back unchanged with `signupModeConfirmed: true`; no secret field is sent, so a stored
 * secret stays. A plain function, like `useSavePlatformSettings`.
 */
export function useConfirmSignupMode() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const save = useSavePlatformSettings();
  return async (): Promise<void> => {
    const s = await unwrap(client.GET('/api/v1/platform/settings'));
    await save({
      mailProvider: s.mailProvider,
      postmarkFrom: s.postmarkFrom,
      postmarkMessageStream: s.postmarkMessageStream,
      smtpHost: s.smtpHost,
      smtpPort: s.smtpPort,
      smtpSecurity: s.smtpSecurity,
      smtpUsername: s.smtpUsername,
      smtpFromAddress: s.smtpFromAddress,
      smtpFromName: s.smtpFromName,
      signupMode: s.signupMode,
      signupModeConfirmed: true,
    });
    await qc.invalidateQueries({ queryKey: keys.onboarding(org) });
  };
}

/** The org with its time zone and default team (the session holds only the name). */
export function useOrgDetail() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.orgDetail(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}', { params: { path: { org } } })),
    staleTime: 60_000,
    // A refetch on focus would reset the form.
    refetchOnWindowFocus: false,
  });
}

/** Change the name, time zone or default team. The sidebar reads the name from the session, so that follows. */
export function useUpdateOrg() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: UpdateOrgInput) =>
      unwrap(client.PATCH('/api/v1/orgs/{org}', { params: { path: { org } }, body: input })),
    onSuccess: (saved: Org) => {
      qc.setQueryData(keys.orgDetail(org), saved);
      qc.setQueryData<Session | null>(keys.session, (s) =>
        s ? { ...s, orgs: s.orgs.map((o) => (o.slug === org ? { ...o, name: saved.name } : o)) } : s,
      );
      void qc.invalidateQueries({ queryKey: keys.onboarding(org) });
    },
  });
}

/** Every org the caller belongs to, with its `avatarUrl` (LAUN-22). Off unless `enabled`. */
export function useOrgList(enabled: boolean) {
  return useQuery({
    queryKey: keys.orgList,
    queryFn: () => unwrap(client.GET('/api/v1/orgs')).then((r) => r.items),
    enabled,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
  });
}

export type AvatarType = 'image/png' | 'image/jpeg' | 'image/gif' | 'image/webp';

/** Set or replace the org's logo (LAUN-22): prepare, PUT the bytes to the store, confirm. */
export function useSetOrgAvatar() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: async (file: File) => {
      const prep = await unwrap(
        client.POST('/api/v1/orgs/{org}/avatar/prepare', {
          params: { path: { org } },
          body: { contentType: file.type as AvatarType, sizeBytes: file.size },
        }),
      );
      const put = await fetch(prep.uploadUrl, { method: 'PUT', headers: prep.headers, body: file });
      if (!put.ok) throw new Error('upload failed');
      return unwrap(
        client.PUT('/api/v1/orgs/{org}/avatar', {
          params: { path: { org } },
          body: { uploadKey: prep.uploadKey },
        }),
      );
    },
    onSuccess: (saved: Org) => afterOrgAvatar(qc, org, saved),
  });
}

/** Remove the org's logo. */
export function useClearOrgAvatar() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: () => unwrap(client.DELETE('/api/v1/orgs/{org}/avatar', { params: { path: { org } } })),
    onSuccess: (saved: Org) => afterOrgAvatar(qc, org, saved),
  });
}

function afterOrgAvatar(qc: QueryClient, org: string, saved: Org) {
  qc.setQueryData(keys.orgDetail(org), saved);
  void qc.invalidateQueries({ queryKey: keys.orgList });
}

// ---------- invites, sign-up requests, platform admins ----------

/** Invites of the org. The list never holds a token. */
export function useInvites() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.invites(org),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/invites', { params: { path: { org }, query: { limit: 200 } } }),
      ).then((r) => r.items),
    staleTime: 15_000,
  });
}

/**
 * Create an invite. A plain function, NOT useMutation: without working mail the reply holds the invite link
 * (`secret`, `url`), and the mutation cache would keep it for minutes. The caller keeps it in state only.
 */
export function useCreateInvite() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return async (input: InviteInput): Promise<CreatedInvite> => {
    const created = await unwrap(
      client.POST('/api/v1/orgs/{org}/invites', { params: { path: { org } }, body: input }),
    );
    void qc.invalidateQueries({ queryKey: keys.invites(org) });
    return created;
  };
}

export function useRevokeInvite() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(client.DELETE('/api/v1/orgs/{org}/invites/{invite}', { params: { path: { org, invite: id } } })),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.invites(org) }),
  });
}

/** Pending sign-up requests. Admins only; the server hides requests that wait for their confirm code. */
export function useSignups() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.signups(org),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/signups', {
          params: { path: { org }, query: { status: 'pending', limit: 200 } },
        }),
      ).then((r) => r.items),
    staleTime: 15_000,
  });
}

export function useApproveSignup() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    // The body is the team access (decisions-T20 #29); none is `all`.
    mutationFn: ({ id, body }: { id: string; body?: SignupApproveInput }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/signups/{signup}/approve', {
          params: { path: { org, signup: id } },
          body,
        }),
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.signups(org) });
      void qc.invalidateQueries({ queryKey: keys.members(org) });
    },
  });
}

export function useRejectSignup() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (v: { id: string; reason?: string; block: boolean }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/signups/{signup}/reject', {
          params: { path: { org, signup: v.id } },
          body: { ...(v.reason ? { reason: v.reason } : {}), block: v.block },
        }),
      ),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.signups(org) }),
  });
}

/** Delete every request that waits for its confirm code. The reply is a count. */
export function usePurgeUnconfirmed() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: () =>
      unwrap(client.DELETE('/api/v1/orgs/{org}/signups/unconfirmed', { params: { path: { org } } })),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.signups(org) }),
  });
}

export function usePlatformAdmins(enabled = true) {
  return useQuery({
    queryKey: keys.platformAdmins,
    enabled,
    queryFn: () => unwrap(client.GET('/api/v1/platform/admins')).then((r) => r.items),
    staleTime: 15_000,
    refetchOnWindowFocus: false,
  });
}

export function useAddPlatformAdmin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (user: string) => unwrap(client.POST('/api/v1/platform/admins', { body: { user } })),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.platformAdmins }),
  });
}

export function useRemovePlatformAdmin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(client.DELETE('/api/v1/platform/admins/{user}', { params: { path: { user: id } } })),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.platformAdmins }),
  });
}

// ---------- issues ----------

export function useIssuesInfinite(filters: IssueFilters, extra: Partial<IssueApiQuery> = {}) {
  const org = useOrgSlug();
  const q = toApiQuery(filters, extra);
  return useInfiniteQuery({
    queryKey: keys.issueList(org, q as Record<string, unknown>),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/issues', {
          params: { path: { org }, query: { ...q, limit: PAGE, cursor: pageParam } },
        }),
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
}

export function useSubIssues(identifier: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.issueList(org, { parent: identifier }),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/issues', {
          params: { path: { org }, query: { parent: identifier, limit: 100 } },
        }),
      ).then((r) => r.items),
  });
}

/** `team` (a team key) limits the search to that team (LAUN-114); empty searches every team. */
export function useIssueSearch(text: string, enabled = true, team = '') {
  const org = useOrgSlug();
  const t = text.trim();
  return useQuery({
    queryKey: keys.issueSearch(org, t, team),
    enabled: enabled && t.length > 0,
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/issues', {
          params: { path: { org }, query: { query: t, limit: 8, ...(team ? { team } : {}) } },
        }),
      ).then((r) => r.items),
    staleTime: 10_000,
  });
}

export function useIssue(identifier: string, placeholder?: Issue) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.issue(org, identifier),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/issues/{issue}', {
          params: {
            path: { org, issue: identifier },
            query: { include: 'relations,stateHistory,attachments,changeRequests' },
          },
        }),
      ),
    placeholderData: placeholder,
  });
}

/** The activity read stops here (20 pages of 250). The rows come oldest first, so a longer history loses the NEWEST. */
export const ACTIVITY_MAX_PAGES = 20;
export const ACTIVITY_PAGE = 250;

/**
 * The recorded changes of an issue (LAUN-49), oldest first, read to the end up to ACTIVITY_MAX_PAGES pages;
 * `truncated` says the end was not reached (the page then shows a note). The key sits under `issueDetails`, so
 * an `issue.updated` event refreshes it.
 */
export function useIssueActivity(identifier: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.issueActivity(org, identifier),
    queryFn: async () => {
      const items: ActivityEntry[] = [];
      let cursor: string | undefined;
      let truncated = false;
      for (let page = 0; page < ACTIVITY_MAX_PAGES; page++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/issues/{issue}/activity', {
            params: {
              path: { org, issue: identifier },
              query: { limit: ACTIVITY_PAGE, ...(cursor ? { cursor } : {}) },
            },
          }),
        );
        items.push(...r.items);
        if (!r.nextCursor) break;
        cursor = r.nextCursor;
        if (page === ACTIVITY_MAX_PAGES - 1) truncated = true;
      }
      return { items, truncated };
    },
  });
}

export function useComments(identifier: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.comments(org, identifier),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/issues/{issue}/comments', {
          params: { path: { org, issue: identifier } },
        }),
      ).then((r) => r.items),
  });
}

// ---------- cache helpers ----------

type AnyData = InfiniteData<IssueList> | IssueList | Issue | Issue[] | undefined;

/** The activity of an issue sits under `issueDetails` too, but it is a list of ActivityEntry, not issues. */
const notActivity = (q: { queryKey: readonly unknown[] }) => q.queryKey[4] !== 'activity';

function mapIssuesIn(data: AnyData, fn: (i: Issue) => Issue): AnyData {
  if (!data) return data;
  if (Array.isArray(data)) return data.map(fn);
  if ('pages' in data) return { ...data, pages: data.pages.map((p) => ({ ...p, items: p.items.map(fn) })) };
  if ('items' in data) return { ...data, items: data.items.map(fn) };
  return fn(data);
}

export function patchIssueInCaches(qc: QueryClient, org: string, id: string, patch: Partial<Issue>): void {
  const apply = (i: Issue): Issue => (i.id === id ? { ...i, ...patch } : i);
  qc.setQueriesData<AnyData>({ queryKey: keys.issues(org) }, (d) => mapIssuesIn(d, apply));
  qc.setQueriesData<AnyData>({ queryKey: keys.issueDetails(org), predicate: notActivity }, (d) =>
    mapIssuesIn(d, apply),
  );
}

/**
 * LAUN-29: show a board drop at once. Moves the issue next to its neighbour in every cached manual-order
 * list that holds both. A list in another order, or without the neighbour, is left alone (the state
 * patch still applies). The server's answer replaces all of this on the refetch.
 */
export function moveIssueInCaches(
  qc: QueryClient,
  org: string,
  id: string,
  placement: { placeAfter?: string; placeBefore?: string },
): void {
  const neighbour = placement.placeAfter ?? placement.placeBefore;
  qc.setQueriesData<AnyData>(
    {
      queryKey: keys.issues(org),
      predicate: (q) => (q.queryKey[4] as { orderBy?: string } | undefined)?.orderBy === 'manual',
    },
    (d) => {
      if (!d || Array.isArray(d) || !('pages' in d)) return d;
      let moved: Issue | undefined;
      const pages = d.pages.map((p) => {
        const at = p.items.findIndex((i) => i.id === id);
        if (at < 0) return p;
        moved = p.items[at];
        return { ...p, items: p.items.filter((_, k) => k !== at) };
      });
      if (!moved) return d;
      // No neighbour (an empty column, or no card of this team): the server puts a moved card on TOP
      // of its new column, so show it there: before the first other card of the same team and state.
      const top = !neighbour;
      const m = moved as Issue;
      for (let pi = 0; pi < pages.length; pi++) {
        const j = pages[pi]!.items.findIndex((i) =>
          top
            ? i.team.id === m.team.id && i.state.id === m.state.id
            : i.id === neighbour || i.identifier === neighbour,
        );
        if (j < 0) continue;
        const items = [...pages[pi]!.items];
        items.splice(placement.placeAfter ? j + 1 : j, 0, m);
        pages[pi] = { ...pages[pi]!, items };
        return { ...d, pages };
      }
      return d; // the neighbour is not loaded in this list
    },
  );
}

/**
 * LAUN-167: a state change sets the issue's `updatedAt` on the server, so in a list sorted by Updated (newest first)
 * it belongs at the top. Show it there at once; other sorts (Created, Priority, Manual) are not changed here.
 */
export function touchIssueInCaches(qc: QueryClient, org: string, id: string): void {
  qc.setQueriesData<AnyData>(
    {
      queryKey: keys.issues(org),
      predicate: (q) => (q.queryKey[4] as { orderBy?: string } | undefined)?.orderBy === 'updatedAt',
    },
    (d) => {
      if (!d || Array.isArray(d) || !('pages' in d) || d.pages.length === 0) return d;
      let moved: Issue | undefined;
      const pages = d.pages.map((p) => {
        const at = p.items.findIndex((i) => i.id === id);
        if (at < 0) return p;
        moved = p.items[at];
        return { ...p, items: p.items.filter((_, k) => k !== at) };
      });
      if (!moved) return d;
      pages[0] = { ...pages[0]!, items: [moved, ...pages[0]!.items] };
      return { ...d, pages };
    },
  );
}

export function findIssueInCaches(qc: QueryClient, org: string, id: string): Issue | undefined {
  let found: Issue | undefined;
  const visit = (d: AnyData) => {
    mapIssuesIn(d, (i) => {
      if (i.id === id && (!found || i.version > found.version)) found = i;
      return i;
    });
  };
  for (const [, d] of qc.getQueriesData<AnyData>({
    queryKey: keys.issueDetails(org),
    predicate: notActivity,
  }))
    visit(d);
  for (const [, d] of qc.getQueriesData<AnyData>({ queryKey: keys.issues(org) })) visit(d);
  return found;
}

// ---------- mutations ----------

export interface UpdateVars {
  issue: Issue;
  patch: Omit<UpdateIssueInput, 'version'>;
  /** What the cached issue should look like while the request is in flight. */
  optimistic?: Partial<Issue>;
  /**
   * The version the person's draft started from. A draft save (title, description) sends exactly this,
   * so an edit made elsewhere while the draft was open gives 409 instead of being overwritten.
   * Quick one-click edits leave it out; they use the newest cached version, then follow this tab's own writes.
   */
  baseVersion?: number;
  /** A board drop: the neighbour it lands next to, to move the card in the cached lists at once. */
  placement?: { placeAfter?: string; placeBefore?: string };
  /** LAUN-167: the update changes the state: in a list sorted by Updated the issue goes to the top at once. */
  touch?: boolean;
  /** Skip the compare-and-swap (never used by the UI; kept for tests). */
  noVersion?: boolean;
  /** LAUN-61: a group drop sends several updates in a row and refetches once at the end itself. */
  deferSettle?: boolean;
}

// This tab sends its writes to one issue ONE AT A TIME, in the order they were made (a FIFO per issue).
// Each write follows this tab's own earlier writes: `chain` maps the version a write sent to the version
// the server returned. A draft that started at version 3 stays valid after this tab's own write moved the
// issue 3 -> 4, because nobody else touched it. A step is recorded only when the reply is exactly one
// version above what was sent. A bigger jump means a write by another tab got in between, so nothing is
// recorded and a draft started before it gets a 409. (Known narrow residual: a no-op PATCH that the
// server re-reads after a foreign write also looks like +1. The client never sends known no-ops.)
interface OwnWrites {
  chain: Map<string, Map<number, number>>;
  tails: Map<string, Promise<void>>;
}
const ownWritesByClient = new WeakMap<QueryClient, OwnWrites>();
function ownWritesOf(qc: QueryClient): OwnWrites {
  let w = ownWritesByClient.get(qc);
  if (!w) {
    w = { chain: new Map(), tails: new Map() };
    ownWritesByClient.set(qc, w);
  }
  return w;
}
/** Forget this tab's own writes (on logout, or when another user signed in). */
export function resetOwnWrites(qc: QueryClient): void {
  ownWritesByClient.get(qc)?.chain.clear();
}
/** Follows this tab's own writes from `base`. Stops at the first step this tab did not make. */
export function followOwnWrites(chain: Map<number, number> | undefined, base: number): number {
  let v = base;
  const seen = new Set<number>();
  while (chain?.has(v) && !seen.has(v)) {
    seen.add(v);
    v = chain.get(v)!;
  }
  return v;
}

/** How long a write waits for the one before it. After that it goes on (and may 409). */
export const OWN_WRITE_WAIT_MS = 10_000;
/** Resolves when `p` settles or after `ms`. Returns false on timeout. */
export async function waitBounded(p: Promise<unknown>, ms: number = OWN_WRITE_WAIT_MS): Promise<boolean> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<false>((r) => {
    timer = setTimeout(() => r(false), ms);
  });
  try {
    return await Promise.race([
      p.then(
        () => true,
        () => true,
      ),
      timeout,
    ]);
  } finally {
    clearTimeout(timer);
  }
}

/** True when the patch would change nothing for `cur`. Unknown fields: not a no-op. */
export function isNoopPatch(patch: Omit<UpdateIssueInput, 'version'>, cur: Issue): boolean {
  const keys = Object.keys(patch) as (keyof typeof patch)[];
  if (keys.length === 0) return false;
  const names = new Set(cur.labels.map((l) => l.name));
  return keys.every((k) => {
    const v = patch[k];
    switch (k) {
      case 'state':
        return v === cur.state.name || v === cur.state.id;
      case 'priority':
        return v === cur.priority;
      case 'assignee':
        return v === null ? cur.assignee === null : v === cur.assignee?.username;
      case 'addLabels':
        return (v as string[]).every((n) => names.has(n));
      case 'removeLabels':
        return (v as string[]).every((n) => !names.has(n));
      default:
        return false;
    }
  });
}

/**
 * Delete an issue (LAUN-201). Only a success drops the cached issue: a 403 or 404 must keep what is on screen.
 * The lists, the project counts and the cycle progress are refreshed either way.
 */
export function useDeleteIssue() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (identifier: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/issues/{issue}', {
          params: { path: { org, issue: identifier } },
        }),
      ),
    onSuccess: (_d, identifier) => qc.removeQueries({ queryKey: keys.issue(org, identifier) }),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: keys.issues(org) });
      void qc.invalidateQueries({ queryKey: ['org', org, 'cycle-progress'] });
      void qc.invalidateQueries({ queryKey: keys.projectsAll(org) });
      void qc.invalidateQueries({ queryKey: keys.projectDetails(org) });
    },
  });
}

export function useUpdateIssue() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: async ({ issue, patch, noVersion, baseVersion }: UpdateVars) => {
      // Everything up to the next await runs when the write is made: take our place in the line
      // and remember which session we belong to.
      const own = ownWritesOf(qc);
      const epoch = getSessionEpoch();
      let finish!: () => void;
      const mine = new Promise<void>((r) => (finish = r));
      const prev = own.tails.get(issue.id);
      own.tails.set(issue.id, mine);
      try {
        if (prev) await waitBounded(prev);
        if (getSessionEpoch() !== epoch) throw new SessionChangedError(); // made as another user
        const chain = own.chain.get(issue.id);
        const latest = findIssueInCaches(qc, org, issue.id);
        let version: number;
        if (baseVersion !== undefined) {
          // A draft save: start from the version the draft began at, never from the cache.
          version = followOwnWrites(chain, baseVersion);
        } else {
          const cached = Math.max(issue.version, latest?.version ?? 0);
          version = followOwnWrites(chain, cached);
          // Judge against the issue as the page showed it: the cache already holds the optimistic edit.
          if (!noVersion && isNoopPatch(patch, issue)) return latest ?? issue; // nothing to send
        }
        const data = await unwrap(
          client.PATCH('/api/v1/orgs/{org}/issues/{issue}', {
            params: { path: { org, issue: issue.identifier } },
            body: noVersion ? patch : { ...patch, version },
          }),
        );
        if (data.version === version + 1) {
          let m = own.chain.get(issue.id);
          if (!m) own.chain.set(issue.id, (m = new Map()));
          m.set(version, data.version);
        }
        return data;
      } finally {
        finish();
        if (own.tails.get(issue.id) === mine) own.tails.delete(issue.id);
      }
    },
    onMutate: async ({ issue, optimistic, placement, touch }) => {
      await qc.cancelQueries({ queryKey: keys.issues(org) });
      await qc.cancelQueries({ queryKey: keys.issueDetails(org) });
      const snapshot = {
        gen: accessGeneration(qc, org), // a team-loss drop after this: the snapshot is not put back (T20 #76)
        lists: qc.getQueriesData<AnyData>({ queryKey: keys.issues(org) }),
        details: qc.getQueriesData<AnyData>({ queryKey: keys.issueDetails(org), predicate: notActivity }),
      };
      if (optimistic) patchIssueInCaches(qc, org, issue.id, optimistic);
      if (placement) moveIssueInCaches(qc, org, issue.id, placement);
      if (touch) touchIssueInCaches(qc, org, issue.id);
      return snapshot;
    },
    onError: (_e, _v, snapshot) => {
      if (!snapshot) return;
      restoreSnapshot(qc, org, snapshot.gen, [...snapshot.lists, ...snapshot.details]);
    },
    onSuccess: (data) => {
      // Keep version/updatedAt current so the next edit's compare-and-swap is right.
      patchIssueInCaches(qc, org, data.id, { version: data.version, updatedAt: data.updatedAt });
    },
    onSettled: (_d, _e, vars) => {
      if (vars.deferSettle) return;
      void qc.invalidateQueries({ queryKey: keys.issues(org) });
      void qc.invalidateQueries({ queryKey: keys.issueDetails(org) });
      // The assignee is a reason (LAUN-38): the subscription view is computed live by the server.
      void qc.invalidateQueries({ queryKey: keys.issueSubscriptions(org) });
      refreshProjectCounts(qc, org); // the project's issue counts (LAUN-170)
    },
  });
}

export function useCreateIssue() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ input, idempotencyKey }: { input: CreateIssueInput; idempotencyKey: string }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/issues', {
          params: { path: { org }, header: { 'Idempotency-Key': idempotencyKey } },
          body: input,
        }),
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.issues(org) });
      refreshProjectCounts(qc, org);
    },
  });
}

export function useCreateComment(identifier: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({
      body,
      parentId,
      idempotencyKey,
    }: {
      body: string;
      parentId?: string;
      idempotencyKey: string;
    }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/issues/{issue}/comments', {
          params: { path: { org, issue: identifier }, header: { 'Idempotency-Key': idempotencyKey } },
          body: { body, parentId },
        }),
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.comments(org, identifier) });
      // A commenter is a reason (LAUN-38).
      void qc.invalidateQueries({ queryKey: keys.issueSubscription(org, identifier) });
    },
  });
}

export function useEditComment(identifier: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: string }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/comments/{comment}', {
          params: { path: { org, comment: id } },
          body: { body },
        }),
      ),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.comments(org, identifier) }),
  });
}

export function useDeleteComment(identifier: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/comments/{comment}', { params: { path: { org, comment: id } } }),
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.comments(org, identifier) });
      // A commenter is a reason (LAUN-38).
      void qc.invalidateQueries({ queryKey: keys.issueSubscription(org, identifier) });
    },
  });
}

export function useCreateRelation(identifier: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({
      type,
      issue,
      idempotencyKey,
    }: {
      type: RelationType;
      issue: string;
      idempotencyKey: string;
    }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/issues/{issue}/relations', {
          params: { path: { org, issue: identifier }, header: { 'Idempotency-Key': idempotencyKey } },
          body: { type, issue },
        }),
      ),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.issueDetails(org) }),
  });
}

export function useDeleteRelation(identifier: string) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (relation: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/issues/{issue}/relations/{relation}', {
          params: { path: { org, issue: identifier, relation } },
        }),
      ),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.issueDetails(org) }),
  });
}

export function useCreateLabel() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    // `team` makes a team label (decisions-T21 #7); without it the label is org-wide.
    mutationFn: ({ name, team }: { name: string; team?: string }) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/labels', {
          params: { path: { org } },
          body: { name, color: '#95a2b3', ...(team ? { team } : {}) },
        }),
      ),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.labels(org) }),
  });
}

// ---------- notification settings (LAUN-36) ----------

/** The caller's settings, resolved by the server (decisions-T13 #32). The web app never recomputes a value. */
export function useNotificationSettings() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.notificationSettings(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/me/notification-settings', { params: { path: { org } } })),
    staleTime: 60_000,
    // A refetch on focus could put an older view over a change that is being saved.
    refetchOnWindowFocus: false,
  });
}

/**
 * The writes of the page. Plain functions (no mutation cache): every write answers the whole resolved view,
 * which replaces the cached one. A follow answers 204, so the cached list of followed teams is edited.
 */
export function useNotificationWrites() {
  const org = useOrgSlug();
  const qc = useQueryClient();
  const key = keys.notificationSettings(org);
  return {
    saveGlobal: async (input: NotificationSettingsInput): Promise<void> => {
      const saved = await unwrap(
        client.PUT('/api/v1/orgs/{org}/me/notification-settings', { params: { path: { org } }, body: input }),
      );
      qc.setQueryData(key, saved);
    },
    saveTeam: async (team: string, input: NotificationTeamSettingsInput): Promise<void> => {
      const saved = await unwrap(
        client.PUT('/api/v1/orgs/{org}/me/notification-settings/teams/{team}', {
          params: { path: { org, team } },
          body: input,
        }),
      );
      qc.setQueryData(key, saved);
    },
    follow: async (team: { id: string; key: string }, on: boolean): Promise<void> => {
      const path = { params: { path: { org, team: team.key } } };
      await unwrap(
        on
          ? client.PUT('/api/v1/orgs/{org}/me/teams/{team}/follow', path)
          : client.DELETE('/api/v1/orgs/{org}/me/teams/{team}/follow', path),
      );
      qc.setQueryData<NotificationSettings>(key, (old) =>
        old
          ? {
              ...old,
              follows: on
                ? [...new Set([...old.follows, team.id])]
                : old.follows.filter((f) => f !== team.id),
            }
          : old,
      );
    },
  };
}

/** The one-click unsubscribe (LAUN-34). Public: the token in the path is the credential. */
export function unsubscribeEmail(token: string): Promise<void> {
  return unwrap(
    client.POST('/api/v1/notifications/unsubscribe/{token}', { params: { path: { token } } }),
  ).then(() => undefined);
}

// ---------- issue subscription (LAUN-38) ----------

/**
 * The caller's subscription state of one issue. No retry: a 403 (an agent token) or a 404 means "no control",
 * and the page must not wait on it. The server computes the reasons; the web app never does.
 */
export function useIssueSubscription(identifier: string) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.issueSubscription(org, identifier),
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/issues/{issue}/subscription', {
          params: { path: { org, issue: identifier } },
        }),
      ),
    retry: false,
    refetchOnWindowFocus: false,
  });
}

/** `true` subscribes, `false` mutes, `null` removes the stored row (back to the implicit rules). */
export function useSetIssueSubscription(identifier: string) {
  const org = useOrgSlug();
  const qc = useQueryClient();
  const key = keys.issueSubscription(org, identifier);
  return useMutation({
    mutationFn: async (subscribed: boolean | null): Promise<void> => {
      const params = { params: { path: { org, issue: identifier } } };
      if (subscribed === null) {
        await unwrap(client.DELETE('/api/v1/orgs/{org}/issues/{issue}/subscription', params));
        // Not awaited: a failed re-read must not look like a failed save.
        void qc.invalidateQueries({ queryKey: key });
        return;
      }
      const saved = await unwrap(
        client.PUT('/api/v1/orgs/{org}/issues/{issue}/subscription', { ...params, body: { subscribed } }),
      );
      qc.setQueryData(key, saved);
    },
  });
}

// ---------- personal webhooks (LAUN-229, decisions-T40) ----------

/** The caller's own webhooks: the host, never the URL. An agent token gets 403. */
export function usePersonalWebhooks() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.personalWebhooks(org),
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/me/webhooks', { params: { path: { org } } })),
    staleTime: 0,
    retry: false,
  });
}

/** Add a webhook. The URL goes in the body once and is never read back. */
export function useCreatePersonalWebhook() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (input: PersonalWebhookCreate) =>
      unwrap(client.POST('/api/v1/orgs/{org}/me/webhooks', { params: { path: { org } }, body: input })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.personalWebhooks(org) }),
  });
}

/** Change a webhook. An absent field keeps its value; `webhookUrl` replaces the stored URL. */
export function useUpdatePersonalWebhook() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (v: { id: string; patch: PersonalWebhookPatch }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/me/webhooks/{webhook}', {
          params: { path: { org, webhook: v.id } },
          body: v.patch,
        }),
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.personalWebhooks(org) }),
  });
}

/** Remove a webhook and its posts. */
export function useRemovePersonalWebhook() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.DELETE('/api/v1/orgs/{org}/me/webhooks/{webhook}', {
          params: { path: { org, webhook: id } },
        }),
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.personalWebhooks(org) }),
  });
}

/** Post the fixed test message. The outcome is on the webhook, so the list is read again. */
export function useTestPersonalWebhook() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (id: string) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/me/webhooks/{webhook}/test', {
          params: { path: { org, webhook: id } },
        }),
      ),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.personalWebhooks(org) }),
  });
}

/**
 * Start a move export (LAUN-250). A plain function, NOT useMutation: the answer holds the passphrase and the
 * ticket, and the query cache must never keep them. The caller holds them in component state and drops them.
 */
export function startMoveExport(includeAttachments: boolean) {
  return unwrap(client.POST('/api/v1/platform/move/export', { body: { includeAttachments } }));
}
