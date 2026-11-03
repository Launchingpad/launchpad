import { ApiError } from '../../api/errors';

// Fixed sentences for Settings, Products (LAUN-66). No server text reaches the screen.
export const SLUG_RE = /^[a-z]([a-z0-9]|-[a-z0-9])*$/;
export const COLOR_RE = /^#[0-9a-fA-F]{6}$/;

export const NAME_RULE = 'Enter 1 to 200 characters.';
export const SLUG_RULE =
  'Use 1 to 40 characters: a-z, 0-9 and single hyphens. Start with a letter and do not end with a hyphen.';
export const SLUG_TAKEN = 'A product with this slug exists already. Use another slug.';
export const SLUG_NEEDED =
  'This name does not give a slug. Enter a slug that starts with a letter a-z, for example "my-product".';
export const ICON_RULE = 'Enter up to 8 characters.';
export const COLOR_RULE = 'Use a colour like #5e6ad2: a hash and 6 hex digits.';

export const CHANGED = 'The product changed or is archived. Reload and try again.';
export const CAP = 'The org has 200 products. Delete an archived product first.';

export type ProductField = 'name' | 'slug' | 'icon' | 'color';

/** What a failed save means. `field` puts the sentence under that field; no field means a banner. */
export function saveError(
  err: unknown,
  slugSent: boolean,
  verb: string,
): { field?: ProductField; text: string } {
  if (!(err instanceof ApiError)) return { text: `Could not ${verb} the product. Try again.` };
  if (err.status === 409 && err.code === 'version_conflict')
    return { text: 'Someone else changed this product. Close this window and open it again.' };
  if (err.status === 409 && /at most \d+ products/.test(err.message)) return { text: CAP };
  if (err.status === 409 && /already used/.test(err.message)) return { field: 'slug', text: SLUG_TAKEN };
  if (err.status === 409) return { text: CHANGED };
  if (err.status === 400) {
    const m = err.message.toLowerCase();
    if (m.startsWith('name')) return { field: 'name', text: NAME_RULE };
    if (m.startsWith('icon')) return { field: 'icon', text: ICON_RULE };
    if (m.startsWith('color') || m.startsWith('colour')) return { field: 'color', text: COLOR_RULE };
    // "cannot make a slug from this name; send a slug" (a name with no Latin letters, or one that starts with a digit).
    if (m.includes('send a slug') && !slugSent) return { field: 'slug', text: SLUG_NEEDED };
    if (m.includes('slug')) return { field: 'slug', text: SLUG_RULE };
  }
  return { text: actionText(err, `${verb} the product`) };
}

/** Banner text for a failed action (archive, restore, delete, assign). */
export function actionText(err: unknown, what: string): string {
  if (err instanceof ApiError) {
    if (err.status === 403) return `You need admin access to ${what}.`;
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
    if (err.status === 404) return 'This product is gone. Close this window and reload the page.';
    if (err.status === 409) return CHANGED;
  }
  return `Could not ${what}. Try again.`;
}
