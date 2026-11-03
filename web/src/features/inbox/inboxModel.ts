import type { InboxNotification } from '../../api/types';
import type { InboxFilter } from '../../api/notifications';

/** "Someone" stands in for a move made by GitLab, by the system, or by a user who was removed. */
const NOBODY = 'Someone';

/** One line under the issue title, in the style of Linear's inbox: who did what. No comment text (decisions-T13 #29). */
export function summary(n: InboxNotification): string {
  const who = n.actor?.name || n.actor?.username || null;
  const by = who ?? NOBODY;
  switch (n.kind) {
    case 'assigned':
      return `${by} assigned this to you`;
    case 'mention':
      return `${by} mentioned you`;
    case 'comment':
      return `${by} commented`;
    case 'new_issue':
      return `${by} created this issue`;
    case 'state_change': {
      const { fromStateName: from, toStateName: to } = n.data;
      const move = from && to ? `from ${from} to ${to}` : to ? `to ${to}` : '';
      return who ? `${who} moved this ${move}`.trim() : `Moved ${move}`.trim();
    }
    case 'project_update_due':
      return 'A project update is due';
    default:
      return `${by} updated this`;
  }
}

/** Where a row leads and how it is named: an issue (identifier and title) or a project reminder (LAUN-196). */
export function target(n: InboxNotification, org: string): { to: string; ident: string; title: string } {
  if (n.issue)
    return { to: `/${org}/issue/${n.issue.identifier}`, ident: n.issue.identifier, title: n.issue.title };
  if (n.project) return { to: `/${org}/projects/${n.project.id}`, ident: 'Project', title: n.project.name };
  return { to: `/${org}/inbox`, ident: 'Notification', title: '' };
}

export function parseFilter(v: string | null): InboxFilter {
  return v === 'all' || v === 'archived' ? v : 'unread';
}

export const FILTER_LABEL: Record<InboxFilter, string> = {
  unread: 'Unread',
  all: 'All',
  archived: 'Archived',
};

export const EMPTY: Record<InboxFilter, { title: string; text: string }> = {
  unread: { title: 'You are all caught up', text: 'New activity on your issues shows here.' },
  all: {
    title: 'No notifications yet',
    text: 'You hear about assignments, mentions, comments and state changes.',
  },
  archived: { title: 'Nothing archived', text: 'Archived notifications show here.' },
};

/** The badge text: no more than two digits and a plus. */
export function badgeText(count: number): string {
  return count > 99 ? '99+' : String(count);
}
