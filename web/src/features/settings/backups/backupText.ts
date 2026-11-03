import { ApiError } from '../../../api/errors';
import type { BackupChange, BackupRun, BackupStatus } from '../../../api/types';

// LAUN-78. Every sentence on the Backups page is fixed here. No text from the server (an error message, a run's
// `errorText`, a change's `detail`) and no word of a remote host reaches the screen: the server's `errorCode` is a
// code of ours and maps to a sentence below; anything else shows as a plain code (only [a-z0-9_]).

export type ErrorContext =
  | 'load'
  | 'destination'
  | 'enable'
  | 'delete'
  | 'check'
  | 'run'
  | 'recipient'
  | 'recipientCancel'
  | 'recipientRemove'
  | 'settings';

/** A fixed sentence the page itself decides on before it sends a request (LAUN-146). It is shown as it is. */
export class PageBlocked extends Error {}

export const FIRST_PIN_NEEDS_SECRETS =
  'The saved secrets work only with the saved settings. Enter all of them again, or test without typing a secret.';

const NETWORK = 'Could not reach the server. Check your connection and try again.';

/** The fixed sentence for a failed backup call. The server's own words are never used. */
export function backupErrorText(err: unknown, ctx: ErrorContext): string {
  if (err instanceof PageBlocked) return err.message;
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  const { status, code } = err;
  if (status === 0) return NETWORK;
  if (status === 401) return 'You are signed out. Sign in again.';
  if (status === 403) {
    if (ctx === 'recipientCancel')
      return 'Another platform admin must cancel a removal that you scheduled. On an install with one platform admin, run "server backup-recipient-cancel" on the host.';
    if (ctx === 'recipientRemove') return 'You cannot remove an encryption key.';
    if (ctx === 'load')
      return 'The server says you are not a platform admin. If you became one after you signed in, sign out and sign in again.';
    return 'You cannot change the backup settings.';
  }
  if (status === 404) return 'That item does not exist any more. Reload the page.';
  if (status === 429) return 'Too many requests. Wait a minute and try again.';
  if (status === 503 || code === 'backup_unavailable')
    return 'This server cannot do that. It has no BACKUP_ENCRYPTION_KEY, or no driver for this type. Ask the person who runs the install.';
  if (status === 409) {
    if (code === 'destination_busy')
      return 'A backup of this destination is already queued or running. Wait until it ends.';
    switch (ctx) {
      case 'recipient':
        return 'The server did not accept this key. It may already be a recipient, the limit of 5 keys is reached, or its first 12 characters match another key.';
      case 'recipientCancel':
        return 'The server refused. A destination is enabled and this change would leave no active key, or nothing is pending for this key.';
      case 'recipientRemove':
        return 'The server refused. A destination is enabled and this removal would leave no active key, or a removal is already planned for this key.';
      case 'enable':
        return 'This destination cannot be enabled yet. Check these things. The server has BACKUP_ENCRYPTION_KEY. The signing key exists. One encryption key is active now. Every secret is saved and readable. A Dropbox destination also needs its authorisation.';
      case 'run':
        return 'The destination is disabled. Enable it first.';
      case 'check':
        return 'The server cannot check this destination. It may have no driver for this type, or the app key and app secret are not saved.';
      default:
        return 'The server refused this change. Check the fields, then try again. The destination may already exist, or this server has no BACKUP_ENCRYPTION_KEY to store a secret.';
    }
  }
  if (status === 400) {
    if (ctx === 'check')
      return 'The server did not accept these settings for a check. If the destination has a different pin or host key saved, edit it and enter every secret again.';
    return 'The server did not accept these values. Check each field. If you changed the connection settings, enter every stored secret again.';
  }
  if (status === 415) return 'The server did not accept the request. Reload the page.';
  return 'The server had a problem. Try again.';
}

export function loadErrorText(err: unknown): string {
  return backupErrorText(err, 'load');
}

/** A server code is shown only when it is a plain identifier. */
export function safeCode(code: string | null | undefined): string {
  return typeof code === 'string' && /^[a-z0-9_]{1,40}$/.test(code) ? code : '';
}

const DRIVER_CODES: Record<string, string> = {
  dns_failed: 'The host name was not found.',
  connect_refused: 'The host refused the connection, or nothing listens on that port.',
  timeout: 'The connection took too long.',
  stalled: 'The transfer stopped.',
  auth_failed: 'The sign-in details were refused, or the base path does not exist.',
  forbidden: 'The account has no right to do this. It needs write, list and delete rights.',
  not_found: 'The bucket, folder or path was not found.',
  too_large: 'The backup is too large for this destination.',
  quota_exceeded: 'The destination is full or over its quota.',
  clock_skew: 'The clock of this server and the clock of the destination differ by more than 5 minutes.',
  tls_untrusted:
    'The certificate is not trusted. Probe the server and confirm its fingerprint, or use a valid certificate.',
  tls_wrong_host: 'The certificate is for a different host name.',
  tls_pin_mismatch: 'The certificate key changed. Probe the server and confirm the new fingerprint.',
  host_key_changed: 'The SSH host key is not the one saved. Probe the server and confirm the new host key.',
  ssrf_blocked:
    'This address is in a private range that the install does not allow. The person who runs the install sets BACKUP_ALLOW_PRIVATE_CIDRS.',
  server_error: 'The destination answered with an error.',
  secrets_unreadable: 'The stored secrets cannot be opened. Enter them again.',
  instance_changed:
    'This install looks like a copy or a restore. Run "deploy.sh --backup-resume" on the host before backups run here.',
  interrupted: 'The run was interrupted.',
};

/** A fixed sentence for a driver or run code. An unknown code shows as the code itself (plain identifier only). */
export function codeText(code: string | null | undefined): string {
  const c = safeCode(code);
  if (!c) return '';
  return DRIVER_CODES[c] ?? `The run failed (code ${c}).`;
}

const STEP_NAMES: Record<string, string> = {
  probe: 'Look at the server',
  connect: 'Connect',
  auth: 'Sign in',
  write: 'Write a test file',
  read: 'Read it back',
  delete: 'Delete it',
  exchange: 'Exchange the authorisation code',
};

export function stepLabel(name: unknown): string {
  return typeof name === 'string' && STEP_NAMES[name] ? STEP_NAMES[name]! : 'Step';
}

export interface Step {
  name: string;
  ok: boolean;
  code: string;
  kind: string;
  fingerprint: string;
  chainVerified: boolean | null;
}

/** Reads `steps` defensively: unknown shapes become empty fields, never a thrown error. */
export function readSteps(run: Pick<BackupRun, 'steps'> | undefined): Step[] {
  if (!run || !Array.isArray(run.steps)) return [];
  return run.steps.map((s) => ({
    name: typeof s.name === 'string' ? s.name : '',
    ok: s.ok === true,
    code: safeCode(typeof s.code === 'string' ? s.code : ''),
    kind: typeof s.kind === 'string' ? s.kind : '',
    fingerprint:
      typeof s.fingerprint === 'string' && /^[0-9A-Za-z+/=:_-]{1,128}$/.test(s.fingerprint)
        ? s.fingerprint
        : '',
    chainVerified: typeof s.chainVerified === 'boolean' ? s.chainVerified : null,
  }));
}

export const TRIGGERS: Record<BackupRun['trigger'], string> = {
  schedule: 'Scheduled',
  manual: 'Back up now',
  catchup: 'Catch-up',
  retry: 'Retry',
  test: 'Test',
  probe: 'Probe',
  verify: 'Verify',
  exchange: 'Authorise',
};

export const RUN_STATUS: Record<BackupRun['status'], string> = {
  queued: 'Queued',
  waiting_dump: 'Waiting for the dump',
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  interrupted: 'Interrupted',
  canceled: 'Canceled',
};

export const BANNERS: Record<BackupStatus['banners'][number], { text: string; level: 'error' | 'warn' }> = {
  encryption_key_missing: {
    level: 'error',
    text: 'This server has no BACKUP_ENCRYPTION_KEY. It cannot store a backup secret or run a backup. Ask the person who runs the install to set it.',
  },
  key_mismatch: {
    level: 'error',
    text: 'BACKUP_ENCRYPTION_KEY is not the key that sealed the stored secrets. Put the old key back, or enter the secrets again.',
  },
  signing_key_missing: {
    level: 'warn',
    text: 'The signing key does not exist yet. The off-site worker makes it when it starts. You cannot enable a destination before then.',
  },
  signing_key_unreadable: {
    level: 'error',
    text: 'This server cannot open the signing key. Backups fail until the install owner runs "deploy.sh --backup-rotate-signing".',
  },
  no_active_recipient: {
    level: 'warn',
    text: 'No encryption key (recipient) is active now. Add one below. A destination cannot be enabled without one.',
  },
  no_recent_dump: {
    level: 'warn',
    text: 'No local database dump finished in the last 36 hours.',
  },
  backup_stale: {
    level: 'error',
    text: 'A destination has had no good backup for longer than the alert time.',
  },
  backup_run_failed: {
    level: 'error',
    text: 'The newest backup of a destination failed and will not be retried.',
  },
  files_not_backed_up: {
    level: 'warn',
    text: 'The attachments (uploaded files) have no good copy on a destination from the last 48 hours. The database backup is separate.',
  },
  mail_off: {
    level: 'warn',
    text: 'Email is not set up, so no backup mail is sent. Choose a mail provider on the Platform page.',
  },
};

const CHANGE_KINDS: Record<string, string> = {
  destination_added: 'A destination was added.',
  destination_deleted: 'A destination was deleted.',
  destination_renamed: 'A destination was renamed.',
  destination_config_changed: 'The connection settings of a destination changed.',
  destination_secrets_changed: 'The secrets of a destination changed.',
  destination_schedule_changed: 'The schedule of a destination changed.',
  destination_retention_changed: 'The retention of a destination changed.',
  destination_enabled: 'A destination was enabled.',
  destination_disabled: 'A destination was disabled.',
  destination_pin_set: 'The TLS pin of a destination was set.',
  destination_host_key_set: 'The SSH host key of a destination was set.',
  recipient_added: 'An encryption key (recipient) was added. It is held for 24 hours.',
  recipient_removal_scheduled: 'The removal of an encryption key was scheduled.',
  recipient_add_cancelled: 'A pending encryption key was cancelled.',
  recipient_removal_cancelled: 'The removal of an encryption key was cancelled.',
  recipient_removed_by_host: 'An encryption key was removed by a command on the host.',
  recipient_cancelled_by_host: 'A pending key change was cancelled by a command on the host.',
  notice_settings_changed: 'The notice settings changed.',
  signing_key_replaced: 'The signing key was replaced.',
  instance_resumed: 'Backups were resumed on this install with a new install id.',
};

export function changeText(c: Pick<BackupChange, 'kind'>): string {
  return CHANGE_KINDS[c.kind] ?? `A backup setting changed (${safeCode(c.kind) || 'unknown'}).`;
}

/** Bytes as a short size. */
export function sizeText(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return '—';
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

export const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
