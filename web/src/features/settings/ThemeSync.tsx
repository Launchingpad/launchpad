import { useEffect } from 'react';
import { useMyPreferences } from '../../api/queries';
import { applyLook, isLook } from '../../lib/theme';

/**
 * LAUN-235 (decisions-T42-themes.md): shows the theme the server holds for the person. theme-init.js already
 * painted the look this browser remembered; when the server answers with another (chosen on another device),
 * the page switches once and the browser remembers the new one. The Appearance page changes the same cached
 * value, so a choice there and a revert after a refused save both pass through here.
 */
export function ThemeSync() {
  const theme = useMyPreferences().data?.theme;
  useEffect(() => {
    if (isLook(theme)) applyLook(theme);
  }, [theme]);
  return null;
}
