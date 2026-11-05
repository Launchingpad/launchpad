import { ApiError } from '../../api/errors';

/** Fixed sentences for the move export. No server text reaches the screen. */
export function moveExportError(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'Only a platform admin who is a person can make a move file.';
  if (err.status === 409 && err.code === 'move_blocked')
    return 'This server cannot make the file now. A key or the file store is missing, or a stored secret cannot be read. If this server has no file store, clear "Include attachments" and try again.';
  if (err.status === 409 && err.code === 'export_in_progress')
    return 'Another move file is being made. Wait for it to end, then try again.';
  if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  return 'The server had a problem. Try again.';
}

/** The download path must be the export route of this server plus ONE ticket segment: no `..`, `?`, `#` or slash. */
const DOWNLOAD_PATH = /^\/api\/v1\/platform\/move\/export\/[A-Za-z0-9_-]{1,128}$/;
export function isDownloadPath(path: string): boolean {
  return DOWNLOAD_PATH.test(path);
}

/** After the whole file was sent, a lost answer does not tell if the server loaded the file. */
export const RESTORE_UNKNOWN =
  'The result is unknown. The server may have loaded the file. Open the setup page again to check.';

/** True when an error after a full upload says nothing sure about the server: no answer, or a 5xx. */
export function isUnknownOutcome(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 0 || err.status >= 500);
}

/** Fixed sentences for the restore on the setup page. */
export function moveRestoreError(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  switch (err.code) {
    case 'bad_passphrase':
      return 'The passphrase is wrong. Check it and try again.';
    case 'bundle_invalid':
      return 'The file is damaged, cut short or not a move file. Make a new file on the old server.';
    case 'bundle_version_mismatch':
      return 'This server runs another release than the one that made the file. Install the same release as the old server, then try again.';
    case 'move_blocked':
      return 'This server cannot load the file. A key or the file store is missing, a file is already in the store, or the file has no admin who can sign in. Read the server log.';
    case 'validation_failed':
      return 'Enter the passphrase and choose a file.';
  }
  if (err.status === 429) return 'Too many attempts. Wait a moment and try again.';
  return 'The restore failed. Nothing was loaded. Try again. If it fails again, read the server log.';
}
