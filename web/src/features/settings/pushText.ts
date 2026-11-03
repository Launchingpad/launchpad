import { ApiError } from '../../api/errors';

/** The one form the relay URL may take. The server checks the same rules; this is for a quick answer. */
export function relayUrlProblem(raw: string): string | null {
  if (raw.length > 512) return 'The URL is too long. At most 512 characters.';
  if (/\s/.test(raw)) return 'The URL must not contain spaces.';
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return 'This is not a URL. It starts with https:// and names a host.';
  }
  if (u.protocol !== 'https:') return 'The URL must start with https://.';
  if (u.username || u.password) return 'The URL must not hold a user name or password.';
  if (u.search || u.hash || raw.includes('?') || raw.includes('#'))
    return 'The URL must not have a query or a fragment.';
  return null;
}

export function relaySecretProblem(raw: string): string | null {
  if (raw.length > 512) return 'The secret is too long. At most 512 characters.';
  // eslint-disable-next-line no-control-regex -- printable ASCII only, as the server checks
  if (!/^[\x21-\x7e]+$/.test(raw)) return 'The secret may hold printable characters without spaces.';
  return null;
}

/** What a failed push means, by the fixed word the server stores. No relay text reaches the screen. */
export const PUSH_WORDS: Record<string, string> = {
  push_rejected: 'The relay refused the request. Check the relay URL.',
  push_unauthorized: 'The relay did not accept the secret. Enter the secret again.',
  push_rate_limited: 'The relay asked us to slow down. Pushes are retried later.',
  push_unavailable: 'The relay had a problem. Pushes are retried later.',
  push_unreachable: 'The server could not reach the relay.',
  push_blocked: 'The server refused to connect to this address.',
  push_redirect: 'The relay answered with a redirect, which the server does not follow.',
  push_unreadable: 'The stored relay cannot be opened with this server key. Enter it again.',
  push_token_invalid: 'The relay says the device token is no longer valid.',
};

/** A fixed sentence for a failed save, remove or test. */
export function pushErrorText(err: unknown, action: 'save' | 'remove' | 'test'): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'You need admin access to change mobile push.';
  if (err.status === 429) return 'You sent many test pushes. Wait a moment and try again.';
  if (action === 'test') {
    if (err.status === 404) return 'There is no relay to test. Save one first.';
    if (err.code === 'push_send_failed') {
      const word = /push_[a-z_]+/.exec(err.message)?.[0] ?? '';
      return `The test push was not sent. ${PUSH_WORDS[word] ?? 'The relay did not accept it.'}`;
    }
    if (err.status === 409 && /no registered device/.test(err.message))
      return 'You have no registered device. Sign in to the mobile app first.';
    if (err.status === 409) return PUSH_WORDS.push_unreadable!;
    return 'The test push failed. Try again.';
  }
  if (action === 'save') {
    if (err.code === 'encryption_unavailable')
      return 'This server has no SETTINGS_ENCRYPTION_KEY, so it cannot store the relay.';
    if (err.status === 400 && /private, loopback/.test(err.message))
      return 'The relay address is private or internal. Use a public https address.';
    if (err.status === 400 && /local or internal host name/.test(err.message))
      return 'The relay address is a local or internal host name. Use a public https host name.';
    if (err.status === 400 && /old spelling/.test(err.message))
      return 'The relay address is a number in an old spelling. Write a normal host name.';
    if (err.status === 400 && /secret again/.test(err.message))
      return 'The relay host changed. Enter the secret again.';
    if (err.status === 400)
      return 'The relay was not accepted. The URL starts with https://, and a new relay needs a URL and a secret.';
    return 'Could not save the mobile push settings. Try again.';
  }
  return 'Could not remove the relay. Try again.';
}

/** A fixed sentence for a failed device removal. */
export function deviceRemoveText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'You cannot change devices.';
  return 'Could not remove the device. Try again.';
}

/** A fixed sentence for a failed device list. */
export function devicesLoadText(err: unknown): string {
  if (err instanceof ApiError && err.status === 403)
    return 'Agents have no mobile devices. Sign in as a person to see yours.';
  return 'Could not load your devices.';
}

export function platformLabel(p: string): string {
  return p === 'ios' ? 'iPhone or iPad' : p === 'android' ? 'Android' : 'Device';
}
