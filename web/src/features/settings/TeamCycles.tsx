import { useState } from 'react';
import { ApiError } from '../../api/errors';
import { useSetTeamCycles } from '../../api/queries';
import type { Team } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import ui from '../../styles/ui.module.css';
import styles from './settings.module.css';
import dialogStyles from './cyclesDialog.module.css';
import { SettingsToggle } from './SettingsParts';

/** The words for a failed switch. The server is the judge; the text says what to do next. */
export function cyclesErrorText(err: unknown, enabling: boolean): string {
  const what = enabling ? 'turn on' : 'turn off';
  if (err instanceof ApiError) {
    // LAUN-10: the server refuses to turn cycles on while an import session is open and not finished.
    if (err.code === 'import_in_progress')
      return 'An import is running. Finish it or close it in Settings, Import from Linear, then turn cycles on.';
    if (err.status === 403) return `You need admin access to ${what} cycles.`;
    if (err.status === 0)
      return `Could not reach the server. The cycles switch is back as it was. Try again.`;
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return `Could not ${what} cycles. The switch is back as it was. Try again.`;
}

/**
 * The Cycles switch of one team (LAUN-3). An admin gets a switch; a member gets the state as text.
 * Turning on asks first (decisions-T3 #35). Turning off does not ask: nothing is deleted.
 */
export function TeamCyclesControl({
  team,
  canEdit,
  onError,
}: {
  team: Team;
  canEdit: boolean;
  onError: (message: string | null) => void;
}) {
  const set = useSetTeamCycles();
  const [confirming, setConfirming] = useState(false);
  const on = team.cyclesEnabled;

  function apply(enabled: boolean) {
    onError(null);
    set.mutate(
      { team: team.key, enabled },
      { onError: (e) => onError(`${team.key}: ${cyclesErrorText(e, enabled)}`) },
    );
  }

  if (!canEdit) return <span>{on ? 'On' : 'Off'}</span>;
  return (
    <>
      <span className={styles.switchCell}>
        <SettingsToggle
          checked={on}
          label={`Cycles for ${team.key}`}
          disabled={set.isPending}
          onChange={(next) => (next ? setConfirming(true) : apply(false))}
        />
        <span aria-hidden="true">{on ? 'On' : 'Off'}</span>
      </span>
      {confirming && (
        <Dialog title={`Turn on cycles for ${team.key}?`} width={460} onClose={() => setConfirming(false)}>
          <div className={dialogStyles.dialogBody}>
            <p>Import your cycles from Linear first.</p>
            <p>
              If you turn cycles on before the import, the cycle job makes a cycle 1. It then clashes with the
              cycle numbers from Linear.
            </p>
            <p className={ui.hint}>
              The job makes the current cycle and the next cycle at once. It then runs every hour.
            </p>
            <div className={styles.actions}>
              <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={() => setConfirming(false)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.primary}`}
                data-autofocus
                onClick={() => {
                  setConfirming(false);
                  apply(true);
                }}
              >
                Turn on
              </button>
            </div>
          </div>
        </Dialog>
      )}
    </>
  );
}
