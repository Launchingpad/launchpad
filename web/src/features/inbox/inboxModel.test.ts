import { describe, expect, it } from 'vitest';
import type { InboxNotification } from '../../api/types';
import { badgeText, parseFilter, summary, target } from './inboxModel';

const n = (over: Partial<InboxNotification> = {}): InboxNotification => ({
  id: 'n1',
  kind: 'comment',
  createdAt: '2026-10-26T10:00:00Z',
  readAt: null,
  archivedAt: null,
  actor: { id: 'u2', username: 'ann', name: 'Ann' },
  issue: { id: 'i1', identifier: 'ORB-1', title: 'Fix it', teamKey: 'ORB', stateName: 'Todo' },
  project: null,
  data: {},
  ...over,
});

describe('summary', () => {
  it('says who did what for each kind', () => {
    expect(summary(n({ kind: 'assigned' }))).toBe('Ann assigned this to you');
    expect(summary(n({ kind: 'mention' }))).toBe('Ann mentioned you');
    expect(summary(n({ kind: 'comment' }))).toBe('Ann commented');
    expect(summary(n({ kind: 'new_issue' }))).toBe('Ann created this issue');
  });
  it('names both states of a move, and works with no actor', () => {
    const data = { fromStateName: 'Todo', toStateName: 'Done' };
    expect(summary(n({ kind: 'state_change', data }))).toBe('Ann moved this from Todo to Done');
    expect(summary(n({ kind: 'state_change', data, actor: null }))).toBe('Moved from Todo to Done');
    expect(summary(n({ kind: 'state_change', data: { toStateName: 'Done' }, actor: null }))).toBe(
      'Moved to Done',
    );
  });
  it('does not fail for a removed user', () => {
    expect(summary(n({ kind: 'mention', actor: null }))).toBe('Someone mentioned you');
  });
});

it('parses the filter from the address and defaults to unread', () => {
  expect(parseFilter('all')).toBe('all');
  expect(parseFilter('archived')).toBe('archived');
  expect(parseFilter('bogus')).toBe('unread');
  expect(parseFilter(null)).toBe('unread');
});

it('caps the badge', () => {
  expect(badgeText(7)).toBe('7');
  expect(badgeText(99)).toBe('99');
  expect(badgeText(100)).toBe('99+');
});

describe('project reminder rows (LAUN-196)', () => {
  const pn = n({
    kind: 'project_update_due',
    actor: null,
    issue: null,
    project: { id: 'p1', name: 'Launch' },
  });
  it('says an update is due and names the project', () => {
    expect(summary(pn)).toBe('A project update is due');
    expect(target(pn, 'orbit')).toEqual({ to: '/orbit/projects/p1', ident: 'Project', title: 'Launch' });
  });
  it('leads an issue row to the issue', () => {
    expect(target(n(), 'orbit')).toEqual({ to: '/orbit/issue/ORB-1', ident: 'ORB-1', title: 'Fix it' });
  });
});
