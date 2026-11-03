import type { ReactNode } from 'react';
import { useProducts } from '../../api/queries';
import type { Team } from '../../api/types';
import ui from '../../styles/ui.module.css';
import products from './products.module.css';
import styles from './settings.module.css';

/**
 * The team checklist of a "Selected teams" choice (decisions-T20 #74, #75): one box per team, a "Select all teams
 * of <product>" button per product with teams (#70), and rows for `extra` keys the list does not show (a team of
 * the member that is not in `GET /teams`), so a save never drops one unseen. Values are team keys. The caller
 * loads the teams and shows their loading and error states. Used by the members dialog and the invite form.
 */
export function TeamChecklist({
  teams,
  picked,
  onChange,
  extra = [],
  invalid,
  describedBy,
}: {
  teams: Team[];
  picked: string[];
  onChange: (next: string[]) => void;
  extra?: string[];
  invalid?: boolean;
  describedBy?: string;
}) {
  const productList = useProducts();
  const withTeams = (productList.data ?? []).filter((p) => p.teamKeys.length > 0);
  const toggle = (key: string, on: boolean) =>
    onChange(on ? [...new Set([...picked, key])] : picked.filter((k) => k !== key));
  const box = (key: string, label: ReactNode) => (
    <label key={key} className={products.check}>
      <input
        type="checkbox"
        checked={picked.includes(key)}
        onChange={(e) => toggle(key, e.target.checked)}
        aria-invalid={invalid || undefined}
        aria-describedby={describedBy}
      />
      <span>{label}</span>
    </label>
  );
  return (
    <>
      {withTeams.length > 0 && (
        <div className={styles.rowActions}>
          {withTeams.map((p) => (
            <button
              key={p.id}
              type="button"
              className={`${ui.btn} ${ui.ghost}`}
              onClick={() => onChange([...new Set([...picked, ...p.teamKeys])])}
            >
              Select all teams of {p.name}
            </button>
          ))}
        </div>
      )}
      <div className={products.checkList} role="group" aria-label="Teams">
        {teams.map((t) =>
          box(
            t.key,
            <>
              <code>{t.key}</code> {t.name}
            </>,
          ),
        )}
        {extra
          .filter((k) => !teams.some((t) => t.key === k))
          .map((k) =>
            box(
              k,
              <>
                <code>{k}</code> (not in the team list)
              </>,
            ),
          )}
      </div>
    </>
  );
}
