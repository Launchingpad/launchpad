import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { client, unwrap } from '../../api/client';
import { useOrgSlug } from '../../api/org';
import type { Issue, Label } from '../../api/types';
import { LabelDot } from '../../ui/LabelPill';
import { PickerList, type ListProps, type PickerOption } from '../../ui/Picker';

export const FREQUENT_MAX = 3;
export const SUGGEST_MAX = 5;
/** A label must be on at least this many issues to count as "frequently used". */
export const FREQUENT_MIN = 2;
/** How many issues of the project or the parent are read for Suggestions. */
export const RELATED_LIMIT = 100;

export type UsageIssue = Pick<Issue, 'id' | 'identifier' | 'projectId' | 'labels'> & {
  parent?: { identifier: string } | null;
};

/**
 * The issues that share this issue's project, and the sub-issues of its parent, only while a label picker
 * is open. They feed Suggestions only; the counts for "Frequently used" come from the server's `issueCount`
 * (LAUN-101, decisions-T21 #12), so there is no sample of the team's issues any more.
 */
export function useRelatedIssues(projectId: string | null | undefined, parent: string | null | undefined) {
  const org = useOrgSlug();
  const get = (query: { project?: string; parent?: string }) =>
    unwrap(
      client.GET('/api/v1/orgs/{org}/issues', {
        params: { path: { org }, query: { ...query, limit: RELATED_LIMIT } },
      }),
    ).then((r) => r.items as unknown as UsageIssue[]);
  const byProject = useQuery({
    // Not under the ['org', o, 'issues'] prefix: that prefix holds list pages the optimistic helpers edit.
    queryKey: ['org', org, 'label-related', 'project', projectId ?? ''],
    enabled: !!projectId,
    staleTime: 5 * 60_000,
    queryFn: () => get({ project: projectId! }),
  });
  const byParent = useQuery({
    queryKey: ['org', org, 'label-related', 'parent', parent ?? ''],
    enabled: !!parent,
    staleTime: 5 * 60_000,
    queryFn: () => get({ parent: parent! }),
  });
  return useMemo(
    () => [...(byProject.data ?? []), ...(byParent.data ?? [])],
    [byProject.data, byParent.data],
  );
}

const byCountThenName = (count: Map<string, number>, names: Map<string, string>) => (a: string, b: string) =>
  (count.get(b) ?? 0) - (count.get(a) ?? 0) || (names.get(a) ?? '').localeCompare(names.get(b) ?? '');

function tally(issues: UsageIssue[]) {
  const n = new Map<string, number>();
  for (const i of issues) for (const l of i.labels) n.set(l.id, (n.get(l.id) ?? 0) + 1);
  return n;
}

export interface LabelContext {
  /** The issue being edited (its own labels are not evidence). Absent in the create dialog. */
  issueId?: string;
}

/**
 * The picker's four groups, in this order, each label in ONE of them. They never depend on what is
 * ticked, so ticking a row never moves it.
 *  1. Suggestions: the labels most used on `related` (issues of the same project, sub-issues of the same
 *     parent), at most 5. None when `related` is empty.
 *  2. Frequently used: the 3 labels with the highest server `issueCount`, at least 2 each. The list the
 *     picker gets is scoped by the issue's team, so the counts are that team's (decisions-T21 #5, #6).
 *  3. Team labels: the rest with a `teamId`, by name.
 *  4. Org labels: the rest without one, by name.
 */
export function labelSections(
  labels: Label[],
  related: UsageIssue[],
  ctx: LabelContext,
): { suggestions: Label[]; frequent: Label[]; team: Label[]; rest: Label[] } {
  const names = new Map(labels.map((l) => [l.id, l.name]));
  const by = new Map(labels.map((l) => [l.id, l]));
  const relatedCount = tally(related.filter((i) => i.id !== ctx.issueId));
  const suggestIds = [...relatedCount.keys()]
    .filter((id) => by.has(id))
    .sort(byCountThenName(relatedCount, names))
    .slice(0, SUGGEST_MAX);
  const count = new Map(labels.map((l) => [l.id, l.issueCount]));
  const frequentIds = labels
    .filter((l) => !suggestIds.includes(l.id) && l.issueCount >= FREQUENT_MIN)
    .map((l) => l.id)
    .sort(byCountThenName(count, names))
    .slice(0, FREQUENT_MAX);
  const taken = new Set([...suggestIds, ...frequentIds]);
  const left = labels.filter((l) => !taken.has(l.id)).sort((a, b) => a.name.localeCompare(b.name));
  return {
    suggestions: suggestIds.map((id) => by.get(id)!),
    frequent: frequentIds.map((id) => by.get(id)!),
    team: left.filter((l) => l.teamId),
    rest: left.filter((l) => !l.teamId),
  };
}

/**
 * The label picker's list: search, grouped rows with a colour dot, a box on the focused or ticked row.
 * `idOf` says what an option's id is (the issue page uses the label id, the create dialog the name).
 */
export function LabelList({
  list,
  labels,
  idOf,
  issueId,
  projectId,
  parent,
}: {
  list: ListProps;
  /** From `useLabels(team)`: with a team, the org labels and that team's, with that team's counts. */
  labels: Label[];
  idOf: (l: Label) => string;
  projectId?: string | null;
  /** The parent issue's identifier. */
  parent?: string | null;
} & LabelContext) {
  const related = useRelatedIssues(projectId, parent);
  const options = useMemo<PickerOption[]>(() => {
    const g = labelSections(labels, related, { issueId });
    const mk = (l: Label, section: string): PickerOption => ({
      id: idOf(l),
      label: l.name,
      icon: <LabelDot color={l.color} />,
      // A team label shows its team key (useful when the list holds every team's labels).
      hint: l.teamKey ?? undefined,
      section,
    });
    return [
      ...g.suggestions.map((l) => mk(l, 'Suggestions')),
      ...g.frequent.map((l) => mk(l, 'Frequently used')),
      ...g.team.map((l) => mk(l, 'Team labels')),
      ...g.rest.map((l) => mk(l, 'Org labels')),
    ];
    // idOf is a fixed choice of each caller, not data.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [labels, related, issueId]);
  return <PickerList {...list} options={options} />;
}
