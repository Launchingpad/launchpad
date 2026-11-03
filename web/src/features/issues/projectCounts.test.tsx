import { waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { keys } from '../../api/keys';
import { useCreateIssue, useUpdateIssue } from '../../api/queries';
import { baseRoutes, makeIssue, mockApi } from '../../test/mockApi';
import { renderAt } from '../../test/render';
import { useIssueActions } from './actions';

// LAUN-170: a project answer carries live issue counts, so every issue mutation must refresh the project caches.

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

let update!: ReturnType<typeof useUpdateIssue>;
let create!: ReturnType<typeof useCreateIssue>;
let actions!: ReturnType<typeof useIssueActions>;
function Harness() {
  update = useUpdateIssue();
  create = useCreateIssue();
  actions = useIssueActions();
  return null;
}

function setup() {
  const issue = makeIssue(1, { version: 1 });
  mockApi({
    ...baseRoutes(),
    'PATCH /api/v1/orgs/:org/issues/:issue': () => ({ json: { ...issue, version: 2 } }),
    'POST /api/v1/orgs/:org/issues': () => ({ status: 201, json: issue }),
  });
  const { client } = renderAt(<Harness />, { path: '/orbit/issues' });
  const k = [keys.projectsAll('orbit'), keys.projects('orbit', 'ORB'), keys.project('orbit', 'p1')];
  for (const key of k) client.setQueryData(key, {});
  const stale = () => k.map((key) => client.getQueryState(key)?.isInvalidated);
  return { issue, stale };
}

describe('issue mutations refresh the project issue counts', () => {
  it('useUpdateIssue', async () => {
    const s = setup();
    await update.mutateAsync({ issue: s.issue, patch: { title: 't' } });
    await waitFor(() => expect(s.stale()).toEqual([true, true, true]));
  });

  it('useCreateIssue', async () => {
    const s = setup();
    await create.mutateAsync({ input: { team: 'ORB', title: 't' }, idempotencyKey: 'k1' });
    await waitFor(() => expect(s.stale()).toEqual([true, true, true]));
  });

  it('the bulk move (placeMany)', async () => {
    const s = setup();
    // The team states load first; until then the move finds no state and moves nothing.
    await waitFor(async () => expect(await actions.placeMany([s.issue], 'Todo', {}, true)).toBe(1));
    await waitFor(() => expect(s.stale()).toEqual([true, true, true]));
  });
});
