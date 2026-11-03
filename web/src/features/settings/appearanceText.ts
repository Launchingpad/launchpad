import type { Look, ThemePref } from '../../lib/theme';

// LAUN-235 (decisions-T42-themes.md): the words of Settings > Appearance.
// LAUN-245: the look stored as `linear` is shown as "Default"; the value does not change.

export const THEME_NAME: Record<Look, string> = { linear: 'Default', paper: 'Paper', terminal: 'Terminal' };

export const THEME_HINT: Record<Look, string> = {
  linear: 'The standard look: compact rows, a neutral palette.',
  paper: 'Warm paper and ink: serif headings, airy rows, no shadows.',
  terminal: 'A console: one monospace face, square corners, dense rows.',
};

export const VARIANT_NAME: Record<ThemePref, string> = {
  system: 'Match the system',
  light: 'Light',
  dark: 'Dark',
};

export const APPEARANCE_LEAD =
  'How Launchpad looks for you. Your theme follows you to every device and organisation.';
export const THEME_SECTION_HINT = 'Each theme has a light and a dark variant. The change shows at once.';
export const VARIANT_SECTION_HINT = 'Kept in this browser only.';

export function saveFailedText(prev: Look): string {
  return `Could not save the theme. Launchpad shows ${THEME_NAME[prev]} again.`;
}
