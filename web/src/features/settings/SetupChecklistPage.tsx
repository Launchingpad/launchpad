import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import { isOpen, useOnboarding, type Step } from './onboarding';
import styles from './onboarding.module.css';
import ui from '../../styles/ui.module.css';

const label: Record<Step['status'], string> = {
  done: 'Done',
  todo: 'Not started',
  skipped: 'Skipped',
  soon: 'Coming soon',
  loading: 'Checking',
  review: 'Review',
  error: 'Could not load',
};

/**
 * `/:org/settings/setup`: "Get Launchpad ready" (LAUN-25). Each step says what it is, whether it is done
 * (worked out by the server, never from a click) and links to the real settings page. It has no form of its own.
 */
export function SetupChecklistPage() {
  const org = useOrgSlug();
  const { steps, prefs, retry, setSkipped, confirmSignups, busy, error } = useOnboarding();
  const [focusStep, setFocusStep] = useState<string | null>(null);
  // The Mark reviewed button leaves when the step is done: move focus to the step's title.
  useEffect(() => {
    if (!focusStep) return;
    document.getElementById(`step-${focusStep}`)?.focus();
    setFocusStep(null);
  }, [focusStep]);
  const open = steps.filter(isOpen).length;
  const required = steps.filter((s) => !s.optional);
  const done = required.filter((s) => s.status === 'done').length;
  return (
    <SettingsPage
      title="Get Launchpad ready"
      description="The steps to set up a new install. Each one opens the page where you do it. A step is done when Launchpad sees it, not when you click."
    >
      <div className={styles.touch}>
        <SettingsSection title="Steps">
          {error && (
            <p role="alert" className={styles.problem}>
              {error}
            </p>
          )}
          <p role="status" className={styles.summary}>
            {done} of {required.length} steps done{open > 0 ? `, ${open} still to do` : ''}.
          </p>
          <SettingsCard>
            <ol className={styles.list}>
              {steps.map((s, n) => (
                <li key={s.id} className={styles.step} data-status={s.status}>
                  <span className={`${styles.mark} ${styles[s.status]}`} aria-hidden="true">
                    {s.status === 'done' ? '✓' : n + 1}
                  </span>
                  <div className={styles.text}>
                    <h3 className={styles.name} id={`step-${s.id}`} tabIndex={-1}>
                      {s.title}{' '}
                      <span className={`${styles.badge} ${styles[s.status]}`}>{label[s.status]}</span>
                    </h3>
                    <p className={styles.help}>{s.help}</p>
                    {s.status === 'error' ? (
                      <p className={styles.detail} role="alert">
                        Launchpad could not read this. Try again.
                      </p>
                    ) : (
                      s.detail && s.status !== 'soon' && <p className={styles.detail}>{s.detail}</p>
                    )}
                  </div>
                  <div className={styles.actions}>
                    {s.page && s.status !== 'error' && (
                      <Link
                        to={`/${org}/settings/${s.page}`}
                        className={`${ui.btn} ${s.status === 'todo' ? ui.primary : ''}`}
                        aria-label={`${s.action}: ${s.title}`}
                      >
                        {s.action}
                      </Link>
                    )}
                    {s.status === 'error' && (
                      <button
                        type="button"
                        className={ui.btn}
                        aria-label={`Retry ${s.title}`}
                        onClick={retry}
                      >
                        Retry
                      </button>
                    )}
                    {s.status === 'review' && (
                      <button
                        type="button"
                        className={ui.btn}
                        aria-label={`Mark ${s.title} reviewed`}
                        aria-disabled={busy || undefined}
                        onClick={() => {
                          if (busy) return;
                          void confirmSignups().then((ok) => ok && setFocusStep(s.id));
                        }}
                      >
                        Mark reviewed
                      </button>
                    )}
                    {(s.status === 'todo' || s.status === 'review' || s.status === 'skipped') && (
                      // One button for Skip and Undo skip, so focus stays on it when the answer comes.
                      <button
                        type="button"
                        className={`${ui.btn} ${ui.ghost}`}
                        aria-label={`${s.status === 'skipped' ? 'Undo skip of' : 'Skip'} ${s.title}`}
                        aria-disabled={busy || undefined}
                        onClick={() => setSkipped(s.id, s.status !== 'skipped')}
                      >
                        {s.status === 'skipped' ? 'Undo skip' : 'Skip'}
                      </button>
                    )}
                  </div>
                </li>
              ))}
            </ol>
          </SettingsCard>
        </SettingsSection>
        <SettingsSection>
          <p className={styles.note}>
            A skip is kept for the whole organisation, so every admin sees it. The banner choice is kept in
            this browser. The checklist never blocks the app.
          </p>
          {prefs.dismissed && (
            <button type="button" className={ui.btn} onClick={() => prefs.setDismissed(false)}>
              Show the banner again
            </button>
          )}
        </SettingsSection>
      </div>
    </SettingsPage>
  );
}
