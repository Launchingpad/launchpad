import { ApiError } from '../../api/errors';

/** The event kinds a service can get, in the order of the server list, with one line each (LAUN-198). */
export const CHAT_EVENTS = [
  { value: 'assigned', label: 'Assigned', hint: 'An issue was created with an assignee, or given one.' },
  { value: 'mention', label: 'Mentions', hint: 'A person was mentioned on an issue or a comment.' },
  { value: 'comment', label: 'Comments', hint: 'A new comment on an issue.' },
  { value: 'state_change', label: 'State changes', hint: 'An issue moved to another state.' },
  { value: 'new_issue', label: 'New issues', hint: 'An issue was created.' },
  {
    value: 'project_update_due',
    label: 'Project updates due',
    hint: 'A project lead is due to post an update.',
  },
] as const;

export type ChatEvent = (typeof CHAT_EVENTS)[number]['value'];
export type ChatKind = 'slack' | 'discord' | 'fluxer' | 'custom';

export const CHAT_LIMIT_FALLBACK = 10;

/** The longest service name, in characters. The same number as ChatMaxName in the server (notify/chat.go). */
export const CHAT_MAX_NAME = 40;

/** One line per kind for the picker, the URL field and the check that gives a quick answer before the request. */
export const CHAT_KINDS: {
  value: ChatKind;
  label: string;
  hint: string;
  placeholder: string;
  problem: string;
  test: (url: string) => boolean;
}[] = [
  {
    value: 'slack',
    label: 'Slack',
    hint: 'In Slack, add the Incoming Webhooks app to a channel and copy its URL. Only hooks.slack.com is accepted.',
    placeholder: 'https://hooks.slack.com/services/…',
    problem:
      'This is not a Slack Incoming Webhook URL. It starts with https://hooks.slack.com/services/ and has three parts.',
    test: (u) =>
      /^https:\/\/hooks\.slack\.com\/services\/[A-Za-z0-9_-]{1,64}\/[A-Za-z0-9_-]{1,64}\/[A-Za-z0-9_-]{1,64}$/.test(
        u,
      ),
  },
  {
    value: 'discord',
    label: 'Discord',
    hint: 'In Discord, open the channel settings, then Integrations, then Webhooks, and copy the webhook URL. Only discord.com and discordapp.com are accepted.',
    placeholder: 'https://discord.com/api/webhooks/…',
    problem:
      'This is not a Discord webhook URL. It starts with https://discord.com/api/webhooks/ and has an id and a token.',
    test: (u) =>
      /^https:\/\/(discord|discordapp)\.com\/api\/webhooks\/[0-9]{1,24}\/[A-Za-z0-9_-]{1,128}$/.test(u),
  },
  {
    value: 'fluxer',
    label: 'Fluxer',
    hint: 'In Fluxer, open the channel settings, then Integrations, then Webhooks, and copy the webhook URL. Fluxer takes the same post as Discord. Launchpad refuses private addresses, localhost, single-label names and names that end in .internal, .local, .lan or .home.arpa, and follows no redirect.',
    placeholder: 'https://fluxer.example.com/api/webhooks/…',
    problem:
      'This is not a Fluxer webhook URL. It must start with https://, name a public host, and end in /api/webhooks/ with an id and a token. Private addresses, localhost, single-label names and names that end in .internal, .local, .lan or .home.arpa are not allowed.',
    test: (u) => {
      try {
        const p = new URL(u);
        return (
          p.protocol === 'https:' &&
          p.hostname !== '' &&
          p.username === '' &&
          p.password === '' &&
          p.search === '' &&
          p.hash === '' &&
          /^\/api\/(v1\/)?webhooks\/[0-9]{1,24}\/[A-Za-z0-9_-]{1,128}$/.test(p.pathname)
        );
      } catch {
        return false;
      }
    },
  },
  {
    value: 'custom',
    label: 'Custom webhook',
    hint: 'Any service that accepts a JSON post at a public https address. Launchpad refuses private addresses, localhost, single-label names and names that end in .internal, .local, .lan or .home.arpa, and follows no redirect.',
    placeholder: 'https://chat.example.com/hooks/…',
    problem:
      'This URL was refused. It must start with https:// and name a public host. Private addresses, localhost, single-label names and names that end in .internal, .local, .lan or .home.arpa are not allowed.',
    test: (u) => {
      try {
        const p = new URL(u);
        return (
          p.protocol === 'https:' &&
          p.hostname !== '' &&
          p.username === '' &&
          p.password === '' &&
          p.hash === ''
        );
      } catch {
        return false;
      }
    },
  },
];

export function kindOf(kind: string) {
  return CHAT_KINDS.find((k) => k.value === kind) ?? CHAT_KINDS.find((k) => k.value === 'custom')!;
}

/** What a failed post means, by the fixed word the server stores. No service text reaches the screen. */
export const CHAT_WORDS: Record<string, string> = {
  chat_rejected: 'The service refused the post. Check that the webhook is still valid.',
  chat_gone:
    'The service says the webhook does not exist any more, or its channel is archived. Replace the webhook.',
  chat_rate_limited: 'The service asked us to slow down. Posts are retried later.',
  chat_unavailable: 'The service had a problem. Posts are retried later.',
  chat_unreachable: 'The server could not reach the service.',
  chat_blocked: 'The server refused to connect to this address.',
  chat_redirect: 'The service answered with a redirect, which the server does not follow.',
  chat_unreadable: 'The stored webhook cannot be opened with this server key. Enter it again.',
};

type Action = 'create' | 'save' | 'remove' | 'test';

/** A fixed sentence for a failed create, save, remove or test. */
export function chatErrorText(err: unknown, action: Action, kind: string = 'custom'): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'You need admin access to change the chat services.';
  if (err.status === 429) return 'You sent many test messages. Wait a moment and try again.';
  if (action === 'test') {
    if (err.status === 404) return 'This service no longer exists.';
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
      return `An org may have at most ${CHAT_LIMIT_FALLBACK} chat services.`;
    if (err.status === 404) return 'This service no longer exists.';
    if (err.status === 409) return 'A service with this name exists. Choose another name.';
    if (err.status === 400 && /^teamId/.test(err.message)) return 'Choose a team of this org.';
    if (err.status === 400) return kindOf(kind).problem;
    return action === 'create'
      ? 'Could not add the service. Try again.'
      : 'Could not save the service. Try again.';
  }
  return 'Could not remove the service. Try again.';
}
