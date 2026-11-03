import { useEffect, useState } from 'react';
import { activeJob, useImportActions, useImportSession } from '../../api/importer';
import { Dialog } from '../../ui/Dialog';
import { ErrorState, SkeletonRows } from '../../ui/States';
import ui from '../../styles/ui.module.css';
import { ErrorLine, useAct } from './ImportParts';
import { CyclesStep, DryRunStep, ExportStep, RunStep } from './ImportSteps';
import { UserMapStep } from './ImportUserMap';
import styles from './import.module.css';
import settings from './settings.module.css';
import { STEP_NAMES, stepOffReason, stepsFor, type Step } from './importText';
import { SettingsCard, SettingsPage } from './SettingsParts';

const STEPS: Step[] = [1, 2, 3, 4, 5];

/**
 * Settings, Import from Linear (LAUN-10, decisions-T17 #48). Five steps. The server session decides which
 * step opens, so a reload resumes the view: `GET /import/sessions/current` is the only source of truth.
 */
export function ImportPage() {
  const q = useImportSession();
  const session = q.data ?? null;
  const { current, max } = stepsFor(session);
  const [viewed, setViewed] = useState<Step | null>(null);
  const [closing, setClosing] = useState(false);

  // A step change that the server made (a job ended, a state moved) takes the view with it.
  useEffect(() => setViewed(null), [current, session?.id]);
  const step = viewed && viewed <= max ? viewed : current;
  const running = activeJob(session);

  return (
    <SettingsPage
      title="Import from Linear"
      description="Copy your Linear workspace into Launchpad: issues, comments, projects, cycles and files. Linear is only read."
      wide
    >
      {q.isPending ? (
        <SkeletonRows n={4} />
      ) : q.isError && !q.data && q.data !== null ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load the import" />
      ) : (
        <div className={styles.page}>
          {q.isError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner} ${styles.banner}`}>
              Could not refresh the import. The page tries again.
            </p>
          )}
          {running && (
            <p className={`${ui.banner} ${styles.banner}`} data-testid="job-banner">
              An import job is running: {running.kind.replace('_', ' ')}. You can leave this page. Come back
              here to see it.
            </p>
          )}
          <ol className={styles.stepper} aria-label="Import steps">
            {STEPS.map((n) => {
              const off = stepOffReason(session, n, max);
              return (
                <li key={n}>
                  <button
                    type="button"
                    className={styles.stepBtn}
                    disabled={!!off}
                    title={off ?? undefined}
                    aria-describedby={off ? `step-off-${n}` : undefined}
                    aria-current={n === step ? 'step' : undefined}
                    onClick={() => setViewed(n)}
                  >
                    {n}. {STEP_NAMES[n]}
                  </button>
                  {off && (
                    <span id={`step-off-${n}`} className="sr-only">
                      {off}
                    </span>
                  )}
                </li>
              );
            })}
          </ol>

          <SettingsCard padded>
            <h2 className={settings.subtitle}>
              Step {step}: {STEP_NAMES[step]}
            </h2>
            {step === 1 && <ExportStep session={session} />}
            {step === 2 && session && <UserMapStep session={session} onSaved={() => setViewed(3)} />}
            {step === 3 && session && <DryRunStep session={session} />}
            {step === 4 && session && <RunStep session={session} />}
            {step === 5 && session && <CyclesStep session={session} />}
          </SettingsCard>

          {session && !running && (
            <div className={settings.actions}>
              <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={() => setClosing(true)}>
                Close this import
              </button>
              <span className={ui.hint}>
                Closing deletes the export files from the server. Rows already imported stay.
              </span>
            </div>
          )}
          {closing && session && <CloseDialog id={session.id} onDone={() => setClosing(false)} />}
        </div>
      )}
    </SettingsPage>
  );
}

export function CloseDialog({ id, onDone }: { id: string; onDone: () => void }) {
  const actions = useImportActions();
  const { busy, error, act } = useAct();
  return (
    <Dialog title="Close this import?" width={460} onClose={onDone}>
      <div className={styles.dialogForm}>
        <p>
          This deletes the export from the server and ends the session. Rows that an import already saved
          stay. To import again you export from Linear again.
        </p>
        <ErrorLine text={error} />
        <div className={settings.actions}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} data-autofocus onClick={onDone}>
            Keep it
          </button>
          <button
            type="button"
            className={`${ui.btn} ${ui.danger}`}
            disabled={busy}
            onClick={() => void act(() => actions.close(id)).then((ok) => ok && onDone())}
          >
            Close the import
          </button>
        </div>
      </div>
    </Dialog>
  );
}
