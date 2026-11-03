import { useQueryClient } from '@tanstack/react-query';
import { accessGeneration, restoreSnapshot } from '../../api/accessGen';
import { useCallback } from 'react';
import { useOrgSlug } from '../../api/org';
import { keys } from '../../api/keys';
import { refreshProjectCounts } from '../../api/events';
import {
  moveIssueInCaches,
  patchIssueInCaches,
  touchIssueInCaches,
  useAllStates,
  useUpdateIssue,
  type UpdateVars,
} from '../../api/queries';
import { ApiError, errorMessage, isConflict, isStalePlacement } from '../../api/errors';
import type { Issue, Label, Member } from '../../api/types';
import { priorityLabel } from '../../lib/priority';
import { useToast } from '../../ui/Toast';

type Placement = { placeAfter?: string; placeBefore?: string };

/**
 * LAUN-236: the undo of the board and the list hears about each write here (useIssueUndo.tsx). `begin` runs when an action starts,
 * before any await, and returns a token (or null: do not record); `applied` gets the issue before and the
 * issue the server returned; `end` runs once the action is over, whatever the outcome.
 */
export interface WriteRecorder {
  begin(): object | null;
  applied(token: object, before: Issue, after: Issue, patch: UpdateVars['patch']): void;
  end(token: object): void;
}

/** Quick edits on an issue, with optimistic updates and a clear message on a version conflict. */
export function useIssueActions(onConflict?: (issue: Issue) => void, recorder?: WriteRecorder) {
  const update = useUpdateIssue();
  const { states } = useAllStates();
  const toast = useToast();
  const org = useOrgSlug(); // actions only make sense inside an org route
  const qc = useQueryClient();

  const run = useCallback(
    async (
      issue: Issue,
      patch: Parameters<typeof update.mutateAsync>[0]['patch'],
      optimistic?: Partial<Issue>,
      placement?: { placeAfter?: string; placeBefore?: string },
      touch?: boolean,
    ): Promise<boolean> => {
      const token = recorder?.begin() ?? null;
      try {
        const after = await update.mutateAsync({ issue, patch, optimistic, placement, touch });
        if (token) recorder!.applied(token, issue, after, patch);
        return true;
      } catch (e) {
        if (isStalePlacement(e)) {
          // The mutation's onSettled has already refetched the lists: the card is back where it was.
          toast.show(
            `The column changed while you moved ${issue.identifier}. It was reloaded; drop it again.`,
            {
              kind: 'error',
            },
          );
        } else if (isConflict(e)) {
          toast.show(`${issue.identifier} changed elsewhere. It was reloaded; your change was not saved.`, {
            kind: 'error',
          });
          onConflict?.(issue);
        } else {
          toast.show(errorMessage(e), { kind: 'error' });
        }
        return false;
      } finally {
        if (token) recorder!.end(token);
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [update.mutateAsync, toast, onConflict, recorder],
  );

  return {
    run,
    setState: (issue: Issue, stateName: string) => {
      if (issue.state.name === stateName) return Promise.resolve(true);
      const st = states.find((s) => s.teamId === issue.team.id && s.name === stateName);
      return run(
        issue,
        { state: stateName },
        st ? { state: { id: st.id, name: st.name, type: st.type, color: st.color } } : undefined,
      );
    },
    /**
     * A board drop (LAUN-29): the new state (when the column is another one) and the place next to a
     * neighbour, in ONE update. With no neighbour (an empty column) the server puts the card on top.
     */
    place: (
      issue: Issue,
      stateName: string,
      placement: { placeAfter?: string; placeBefore?: string },
      // LAUN-171: a drop with Ctrl or Command also sets the priority, in the same update.
      priority?: number,
    ) => {
      const prio = priority !== undefined && priority !== issue.priority ? priority : undefined;
      const moves = issue.state.name !== stateName;
      const st = moves ? states.find((s) => s.teamId === issue.team.id && s.name === stateName) : undefined;
      // LAUN-65 (T18 #27): a board column can hold a state that only some teams have (the product board). A card
      // whose team lacks it is refused here with no PATCH, the server would refuse it too. Only when that team's
      // states have loaded: before that we cannot know, and the server stays the judge.
      if (moves && !st && states.some((s) => s.teamId === issue.team.id)) {
        toast.show(`${issue.identifier} is in team ${issue.team.key}, which has no state "${stateName}".`, {
          kind: 'error',
        });
        return Promise.resolve(false);
      }
      if (!moves && !placement.placeAfter && !placement.placeBefore && prio === undefined)
        return Promise.resolve(true);
      const optimistic: Partial<Issue> = {
        ...(st ? { state: { id: st.id, name: st.name, type: st.type, color: st.color } } : {}),
        ...(prio !== undefined
          ? { priority: prio, priorityLabel: priorityLabel(prio) as Issue['priorityLabel'] }
          : {}),
      };
      return run(
        issue,
        {
          ...(moves ? { state: stateName } : {}),
          ...(prio !== undefined ? { priority: prio } : {}),
          ...placement,
        },
        Object.keys(optimistic).length ? optimistic : undefined,
        placement,
        moves || prio !== undefined,
      );
    },
    /**
     * A board group drop (LAUN-61): the selected cards land as a block, in the order given. ONE PATCH per
     * issue, in a row: the first goes to `base` (the place the drop chose for its team), each later one
     * `placeAfter` the last card of its team that was placed, so the block keeps its order. The cached
     * lists are changed for all of them at once; any failure puts the lists back, then one refetch
     * shows what the server did. Resolves with how many moved.
     */
    placeMany: async (
      issues: Issue[],
      stateName: string,
      base: Record<string, Placement>,
      stateOnly = false, // LAUN-167: not Manual: send the state only, no place
      priority?: number, // LAUN-171: also set this priority on every card
    ) => {
      const prioOf = (i: Issue) => (priority !== undefined && priority !== i.priority ? priority : undefined);
      const token = recorder?.begin() ?? null; // the whole group drop is ONE undo step
      // A throw must still end the batch, or the undo would wait for it for ever.
      try {
        const snap = qc.getQueriesData<unknown>({ queryKey: keys.issues(org) });
        const gen = accessGeneration(qc, org); // a team-loss drop after this: the snapshot is not put back (T20 #76)
        const plan = (done: Map<string, string>, i: Issue): Placement =>
          stateOnly
            ? {}
            : done.has(i.team.id)
              ? { placeAfter: done.get(i.team.id)! }
              : (base[i.team.id] ?? {});
        const stateOf = (i: Issue) =>
          i.state.name === stateName
            ? undefined
            : states.find((s) => s.teamId === i.team.id && s.name === stateName);
        const opt = new Map<string, string>();
        for (const i of issues) {
          const st = stateOf(i);
          const p = prioOf(i);
          if (st)
            patchIssueInCaches(qc, org, i.id, {
              state: { id: st.id, name: st.name, type: st.type, color: st.color },
            });
          if (p !== undefined)
            patchIssueInCaches(qc, org, i.id, {
              priority: p,
              priorityLabel: priorityLabel(p) as Issue['priorityLabel'],
            });
          if (stateOnly) {
            if (st || p !== undefined) touchIssueInCaches(qc, org, i.id); // the sort puts it; Updated: on top
          } else moveIssueInCaches(qc, org, i.id, plan(opt, i));
          opt.set(i.team.id, i.id);
        }
        const placed = new Map<string, string>();
        let ok = 0;
        let first: unknown;
        for (const i of issues) {
          const st = stateOf(i);
          const placement = plan(placed, i);
          if (!st && i.state.name !== stateName) {
            first ??= new Error(`${i.identifier} has no state ${stateName}`);
            continue;
          }
          const moves = i.state.name !== stateName;
          const p = prioOf(i);
          if (!moves && !placement.placeAfter && !placement.placeBefore && p === undefined) {
            ok++;
            placed.set(i.team.id, i.id);
            continue;
          }
          try {
            const patch = {
              ...(moves ? { state: stateName } : {}),
              ...(p !== undefined ? { priority: p } : {}),
              ...placement,
            };
            const after = await update.mutateAsync({
              issue: i,
              patch,
              placement: stateOnly ? undefined : placement,
              deferSettle: true,
            });
            if (token) recorder!.applied(token, i, after, patch);
            ok++;
            placed.set(i.team.id, i.id);
          } catch (e) {
            first ??= e;
            // The column changed, or the session or the network is gone: the rest would fail the same way.
            if (isStalePlacement(e) || !(e instanceof ApiError) || e.status === 401 || e.status === 403)
              break;
          }
        }
        if (ok < issues.length) {
          restoreSnapshot(qc, org, gen, snap);
          const why = isStalePlacement(first) ? 'The column changed; it was reloaded' : errorMessage(first);
          toast.show(`${issues.length - ok} of ${issues.length} issues could not be moved. ${why}`, {
            kind: 'error',
          });
        }
        void qc.invalidateQueries({ queryKey: keys.issues(org) });
        void qc.invalidateQueries({ queryKey: keys.issueDetails(org) });
        void qc.invalidateQueries({ queryKey: keys.issueSubscriptions(org) }); // deferSettle skipped it (LAUN-38)
        refreshProjectCounts(qc, org);
        return ok;
      } finally {
        if (token) recorder!.end(token);
      }
    },
    /**
     * LAUN-171: a board drop in a column that is not a state: set the assignee (a member, or null to clear) and/or the
     * priority in ONE update. Nothing is sent when the issue already has the values.
     */
    setFields: (
      issue: Issue,
      f: { assignee?: Pick<Member, 'id' | 'username' | 'name'> | null; priority?: number },
    ) => {
      const patch: Record<string, unknown> = {};
      const optimistic: Partial<Issue> = {};
      if (f.assignee !== undefined && (f.assignee?.username ?? null) !== (issue.assignee?.username ?? null)) {
        patch.assignee = f.assignee ? f.assignee.username : null;
        optimistic.assignee = f.assignee
          ? { id: f.assignee.id, username: f.assignee.username, name: f.assignee.name }
          : null;
      }
      if (f.priority !== undefined && f.priority !== issue.priority) {
        patch.priority = f.priority;
        optimistic.priority = f.priority;
        optimistic.priorityLabel = priorityLabel(f.priority) as Issue['priorityLabel'];
      }
      if (Object.keys(patch).length === 0) return Promise.resolve(true);
      return run(issue, patch, optimistic, undefined, true);
    },
    setPriority: (issue: Issue, priority: number) =>
      issue.priority === priority
        ? Promise.resolve(true)
        : run(
            issue,
            { priority },
            { priority, priorityLabel: priorityLabel(priority) as Issue['priorityLabel'] },
          ),
    setAssignee: (issue: Issue, m: Member | null) =>
      run(
        issue,
        { assignee: m ? m.username : null },
        { assignee: m ? { id: m.id, username: m.username, name: m.name } : null },
      ),
    addLabel: (issue: Issue, l: Label) =>
      issue.labels.some((x) => x.id === l.id)
        ? Promise.resolve(true)
        : run(
            issue,
            { addLabels: [l.name] },
            { labels: [...issue.labels, { id: l.id, name: l.name, color: l.color }] },
          ),
    removeLabel: (issue: Issue, l: Label) =>
      run(issue, { removeLabels: [l.name] }, { labels: issue.labels.filter((x) => x.id !== l.id) }),
  };
}
