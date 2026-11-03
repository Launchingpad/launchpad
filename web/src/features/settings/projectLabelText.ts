import { ApiError } from '../../api/errors';

// Fixed sentences for Settings, Project labels (LAUN-194). No server text reaches the screen.
export const COLOR_RE = /^#[0-9a-fA-F]{6}$/;
export const NAME_RULE = 'Enter 1 to 100 characters.';
export const COLOR_RULE = 'Use a colour like #5e6ad2: a hash and 6 hex digits.';
export const NAME_TAKEN = 'A project label with this name exists already. Use another name.';
export const DEFAULT_COLOR = '#95a2b3';

export type ProjectLabelField = 'name' | 'color';

/** What a failed save means. `field` puts the sentence under that field; no field means a banner. */
export function saveError(err: unknown, verb: string): { field?: ProjectLabelField; text: string } {
  if (err instanceof ApiError) {
    if (err.status === 409) return { field: 'name', text: NAME_TAKEN };
    if (err.status === 400) {
      const m = err.message.toLowerCase();
      if (m.startsWith('name')) return { field: 'name', text: NAME_RULE };
      if (m.startsWith('color')) return { field: 'color', text: COLOR_RULE };
    }
  }
  return { text: actionText(err, `${verb} the label`) };
}

/** Banner text for a failed action. */
export function actionText(err: unknown, what: string): string {
  if (err instanceof ApiError) {
    if (err.status === 403) return `You need admin access to ${what}.`;
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
    if (err.status === 404) return 'This label is gone. Close this window and reload the page.';
  }
  return `Could not ${what}. Try again.`;
}
