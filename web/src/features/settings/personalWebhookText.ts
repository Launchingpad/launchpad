import { ApiError } from '../../api/errors';
import { CHAT_WORDS, kindOf } from './chatText';

/** The choices of a personal webhook (LAUN-229, decisions-T40 #7), in the order of the server list. */
export const PERSONAL_EVENTS = [
  {
    value: 'assigned',
    label: 'Assigned to me',
    hint: 'An issue is created with you as the assignee, or is given to you.',
  },
  {
    value: 'my_issues',
    label: 'Changes on my issues',
    hint: 'An issue you are assigned to, or that you created, is changed or gets a new comment.',
  },
] as const;

export type PersonalEvent = (typeof PERSONAL_EVENTS)[number]['value'];

export const PERSONAL_LIMIT_FALLBACK = 3;

type Action = 'create' | 'save' | 'remove' | 'test' | 'load';

/** A fixed sentence for a failed request. No server text reaches the screen. */
export function personalErrorText(err: unknown, action: Action, kind: string = 'custom'): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'This account cannot have personal webhooks.';
  if (err.status === 429) return 'You sent many test messages. Wait a moment and try again.';
  if (action === 'load') return 'Could not load your webhooks. Try again.';
  if (action === 'test') {
    if (err.status === 404) return 'This webhook no longer exists.';
    if (err.code === 'chat_send_failed') {
      const word = /chat_[a-z_]+/.exec(err.message)?.[0] ?? '';
      return `The test message was not sent. ${CHAT_WORDS[word] ?? 'The service did not accept it.'}`;
    }
    if (err.status === 409) return CHAT_WORDS.chat_unreadable!;
    return 'The test message failed. Try again.';
  }
  if (action === 'create' || action === 'save') {
    if (err.code === 'encryption_unavailable')
      return 'This server has no SETTINGS_ENCRYPTION_KEY, so it cannot store the webhook.';
    if (err.code === 'integration_limit')
      return `You may have at most ${PERSONAL_LIMIT_FALLBACK} personal webhooks.`;
    if (err.status === 404) return 'This webhook no longer exists.';
    if (err.status === 409) return 'You have a webhook with this name. Choose another name.';
    if (err.status === 400) return kindOf(kind).problem;
    return action === 'create'
      ? 'Could not add the webhook. Try again.'
      : 'Could not save the webhook. Try again.';
  }
  return 'Could not remove the webhook. Try again.';
}
