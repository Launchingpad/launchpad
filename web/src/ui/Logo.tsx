import { useState } from 'react';
import styles from '../styles/ui.module.css';

/**
 * The URL an `<img>` should use for an avatar the API named (LAUN-22): the path and query only, on the
 * page's own origin, so the session cookie goes with it even when PUBLIC_BASE_URL is another origin.
 */
export function avatarSrc(url: string | null | undefined): string | null {
  if (!url) return null;
  try {
    const u = new URL(url, window.location.origin);
    return u.pathname + u.search;
  } catch {
    return null;
  }
}

export interface LogoProps {
  /** The org or project name: the letter of the fallback and the alt text. */
  name: string;
  /** `avatarUrl` from the API; null or absent shows the emoji, else the letter. */
  src?: string | null;
  /** A project's `emoji` (LAUN-232): shown when there is no image (or it does not load), before the letter. */
  emoji?: string | null;
  /** Side in px (default 22). */
  size?: number;
  /** True when the name is written next to the logo: an empty alt, so a reader says the name once. */
  decorative?: boolean;
  className?: string;
}

/**
 * The shared avatar for an org or a project. The order is fixed (decisions-T41): the image; else the
 * project's emoji; else the first letter of the name on an accent tile. A failed image load falls back the
 * same way. Colours are tokens, so dark mode follows.
 */
export function Logo({ name, src, emoji, size = 22, decorative = false, className }: LogoProps) {
  // A failed load shows the emoji or the letter; a new src tries again (the key resets the state).
  const [failed, setFailed] = useState<string | null>(null);
  const url = avatarSrc(src);
  const shown = url && failed !== url ? url : null;
  const letter = ([...name.trim()][0] ?? '?').toUpperCase();
  const glyph = emoji?.trim() || null;
  const kind = shown ? 'image' : glyph ? 'emoji' : 'letter';
  const box = {
    width: size,
    height: size,
    fontSize: Math.max(10, Math.round(size * (kind === 'emoji' ? 0.72 : 0.5))),
    borderRadius: Math.max(5, Math.round(size * 0.27)),
  };
  return (
    <span className={`${styles.logo} ${className ?? ''}`} style={box} data-logo={kind}>
      {shown ? (
        <img
          className={styles.logoImg}
          src={shown}
          alt={decorative ? '' : name}
          width={size}
          height={size}
          onError={() => setFailed(shown)}
        />
      ) : (
        <span
          aria-hidden={decorative ? 'true' : undefined}
          aria-label={decorative ? undefined : name}
          role={decorative ? undefined : 'img'}
        >
          {glyph ?? letter}
        </span>
      )}
    </span>
  );
}
