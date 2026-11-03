import { useId, useState } from 'react';
import { useMyPreferences } from '../../api/queries';
import { LOOKS, useLook, useTheme, type Look, type ThemePref } from '../../lib/theme';
import { StatusIcon } from '../../ui/Icons';
import { SettingsPage, SettingsSection } from './SettingsParts';
import { useChooseTheme } from './useChooseTheme';
import {
  APPEARANCE_LEAD,
  THEME_HINT,
  THEME_NAME,
  THEME_SECTION_HINT,
  VARIANT_NAME,
  VARIANT_SECTION_HINT,
} from './appearanceText';
import styles from './appearance.module.css';

// LAUN-235 (decisions-T42-themes.md). Settings > Appearance, for every member: the theme (stored on the
// server, it follows the person) and the light or dark variant (this browser only, as before). A choice shows
// at once; a refused or failed save puts the theme back and says so.

export function AppearancePage() {
  const shown = useLook();
  // Read the stored value here too, so the page also works where the shell's ThemeSync is not mounted.
  useMyPreferences();
  const { pref, setPref } = useTheme();
  const [error, setError] = useState<string | null>(null);
  const themeHead = useId();
  const variantHead = useId();

  const choose = useChooseTheme(setError);
  function pick(look: Look) {
    setError(null);
    choose(look);
  }

  return (
    <SettingsPage title="Appearance" description={APPEARANCE_LEAD}>
      <SettingsSection title="Theme" id={themeHead} description={THEME_SECTION_HINT}>
        <div role="radiogroup" aria-labelledby={themeHead} className={styles.themes}>
          {LOOKS.map((look) => (
            <label key={look} className={`${styles.option} ${look === shown ? styles.checked : ''}`}>
              <ThemePreview look={look} />
              <span className={styles.choice}>
                <input
                  type="radio"
                  name="ui-theme"
                  value={look}
                  checked={look === shown}
                  onChange={() => pick(look)}
                  aria-labelledby={`${themeHead}-${look}-name`}
                  aria-describedby={`${themeHead}-${look}`}
                />
                <span className={styles.name} id={`${themeHead}-${look}-name`}>
                  {THEME_NAME[look]}
                </span>
              </span>
              <span className={styles.hint} id={`${themeHead}-${look}`}>
                {THEME_HINT[look]}
              </span>
            </label>
          ))}
        </div>
        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
      </SettingsSection>
      <SettingsSection title="Light or dark" id={variantHead} description={VARIANT_SECTION_HINT}>
        <div role="radiogroup" aria-labelledby={variantHead} className={styles.variants}>
          {(['system', 'light', 'dark'] as ThemePref[]).map((p) => (
            <label key={p} className={styles.variant}>
              <input
                type="radio"
                name="ui-variant"
                value={p}
                checked={pref === p}
                onChange={() => setPref(p)}
              />
              {VARIANT_NAME[p]}
            </label>
          ))}
        </div>
      </SettingsSection>
    </SettingsPage>
  );
}

/**
 * A small live picture of a theme: its colours, type and shape come from the real tokens (data-preview, see
 * themes.css), in the variant the page shows. Decoration only: the radio and its name carry the choice.
 */
export function ThemePreview({ look }: { look: Look }) {
  return (
    <span className={styles.preview} data-preview={look} aria-hidden="true">
      <span className={styles.pvSide}>
        <span className={styles.pvNav} />
        <span className={`${styles.pvNav} ${styles.pvNavOn}`} />
        <span className={styles.pvNav} />
      </span>
      <span className={styles.pvMain}>
        <span className={styles.pvHead}>
          <span className={styles.pvTitle}>Issues</span>
          <span className={styles.pvBtn}>New</span>
        </span>
        <span className={`${styles.pvRow} ${styles.pvRowOn}`}>
          <StatusIcon look={look} type="started" color="#f2c94c" name="In Progress" size={12} />
          <span className={styles.pvIdent}>LAUN-142</span>
          <span className={styles.pvText}>Add a quick filter</span>
        </span>
        <span className={styles.pvRow}>
          <StatusIcon look={look} type="unstarted" color="#e2e2e2" name="Todo" size={12} />
          <span className={styles.pvIdent}>LAUN-128</span>
          <span className={styles.pvText}>Cycle chart</span>
        </span>
        <span className={styles.pvRow}>
          <StatusIcon look={look} type="completed" color="#5e6ad2" name="Done" size={12} />
          <span className={styles.pvIdent}>LAUN-117</span>
          <span className={styles.pvText}>Reorder sub-issues</span>
        </span>
      </span>
    </span>
  );
}
