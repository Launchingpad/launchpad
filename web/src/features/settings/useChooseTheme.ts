import { useCallback } from 'react';
import { useSetMyTheme } from '../../api/queries';
import { applyLook, currentLook, type Look } from '../../lib/theme';
import { useToast } from '../../ui/Toast';
import { saveFailedText } from './appearanceText';

/**
 * LAUN-235: choose a theme (the Appearance page and the command palette). It shows at once and is saved; when the
 * latest save fails, the last theme the server accepted comes back and a toast says so (`onFailed` also gets the
 * text, for the page's own error line).
 */
export function useChooseTheme(onFailed?: (text: string) => void) {
  const { choose } = useSetMyTheme();
  const toast = useToast();
  return useCallback(
    (look: Look) => {
      // Read at call time: the palette keeps this function in a memo.
      const shown = currentLook();
      if (look === shown) return;
      applyLook(look);
      choose(look, shown, (back) => {
        applyLook(back);
        const text = saveFailedText(back);
        onFailed?.(text);
        toast.show(text, { kind: 'error' });
      });
    },
    [choose, toast, onFailed],
  );
}
