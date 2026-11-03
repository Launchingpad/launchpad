import { ApiError } from '../../api/errors';
import { useIssueSubscription, useSetIssueSubscription } from '../../api/queries';
import type { IssueSubscription } from '../../api/types';
import ui from '../../styles/ui.module.css';
import detail from './detail.module.css';

// LAUN-38. The Subscribe / Unsubscribe control in the Activity header (decisions-T13 #40). The server says the
// state and the reasons; this file only words them. Every text is fixed: nothing from the server is shown.

const REASON: Record<IssueSubscription['reasons'][number], string> = {
  creator: 'you created it',
  assignee: 'you are the assignee',
  commenter: 'you commented',
  mentioned: 'you were mentioned',
  manual: 'you subscribed',
};

/** The sentence under the button. Exported for the tests. */
export function reasonText(s: IssueSubscription): string {
  if (s.state === 'muted') return 'You muted this issue. You get no notifications about it.';
  if (s.reasons.length === 0) return 'You are not subscribed.';
  return `You are subscribed because ${s.reasons.map((r) => REASON[r]).join(', ')}.`;
}

function failText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not save. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 403) return 'You cannot change this subscription.';
  if (err.status === 404) return 'This issue no longer exists. Reload the page.';
  return 'Could not save. Try again.';
}

export function SubscribeControl({ identifier }: { identifier: string }) {
  const q = useIssueSubscription(identifier);
  const set = useSetIssueSubscription(identifier);
  // No broken control: while loading, or when the server refuses the read (an agent or read-only token: 403),
  // there is nothing to show.
  const s = q.data;
  if (!s) return null;
  const subscribed = s.state !== 'muted' && s.reasons.length > 0;
  // A row the person made (manual, or from a mention) and no implicit reason: removing the row is enough.
  // Otherwise a mute row is needed, because an implicit reason still holds.
  const implicit = s.reasons.some((r) => r === 'creator' || r === 'assignee' || r === 'commenter');
  const onClick = () => set.mutate(subscribed ? (implicit ? false : null) : true);
  return (
    <div className={detail.subscribe}>
      <span className={detail.subText} role="status">
        {reasonText(s)}
      </span>
      {set.isError && (
        <span className={detail.subText} role="alert">
          {failText(set.error)}
        </span>
      )}
      <button
        type="button"
        className={`${ui.btn} ${ui.ghost}`}
        // aria-disabled, not disabled: a disabled button drops keyboard focus while the write runs.
        aria-disabled={set.isPending || undefined}
        aria-busy={set.isPending || undefined}
        onClick={() => {
          if (!set.isPending) onClick();
        }}
      >
        {subscribed ? 'Unsubscribe' : 'Subscribe'}
      </button>
    </div>
  );
}
