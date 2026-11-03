import { useEffect } from 'react';
import { useOrgSlug } from '../api/org';
import { useAuthedSession } from '../features/auth/session';

/** Linear's separator: U+203A with a space on each side. */
export const TITLE_SEPARATOR = ' › ';
export const APP_NAME = 'Launchpad';
/** An issue title in a tab is cut here, with an ellipsis, so tabs stay readable. */
export const MAX_ITEM_TITLE = 60;

/** Cut to `max` characters (code points) at most, the last one an ellipsis. */
export function clip(text: string, max = MAX_ITEM_TITLE): string {
  const chars = [...text.replace(/\s+/g, ' ').trim()];
  return chars.length <= max
    ? chars.join('')
    : chars
        .slice(0, max - 1)
        .join('')
        .trimEnd() + '…';
}

export type TitlePart = string | null | undefined | false;

/** Join the parts that exist. No part: the caller keeps the title that is there. */
export function joinTitle(parts: TitlePart[]): string {
  return parts.filter((p): p is string => !!p).join(TITLE_SEPARATOR);
}

/**
 * Set `document.title` to the parts, joined. It restores nothing on unmount: the next page sets its own
 * (and `index.html` holds the first paint). A place for the unread count, "(3) ", is `prefix`: LAUN-23.
 */
export function useDocumentTitle(parts: TitlePart[], prefix = ''): void {
  const title = prefix + joinTitle(parts);
  useEffect(() => {
    if (title) document.title = title;
  }, [title]);
}

/** The org's display name, from the session (no request), else the slug while the session has no row. */
export function useOrgName(): string {
  const slug = useOrgSlug();
  const { orgs } = useAuthedSession();
  return orgs.find((o) => o.slug === slug)?.name ?? slug;
}

/** A page inside an org: "<Org name> › <page parts>". */
export function usePageTitle(...parts: TitlePart[]): void {
  const org = useOrgName();
  useDocumentTitle([org, ...parts]);
}

/** A page outside any org (login, setup, join, sign-up): "Launchpad › <page>". */
export function useAppTitle(page: string): void {
  useDocumentTitle([APP_NAME, page]);
}
