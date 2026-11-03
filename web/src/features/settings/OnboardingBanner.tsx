import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useIsOrgAdmin } from './access';
import { isOpen, useOnboarding } from './onboarding';
import styles from './onboarding.module.css';
import ui from '../../styles/ui.module.css';

/** A thin bar above the page for an org admin while steps are open. It can be dismissed and never blocks the app. */
export function OnboardingBanner() {
  // The data hooks live in the inner part, so a member makes no extra request.
  return useIsOrgAdmin() ? <AdminBanner /> : null;
}

function AdminBanner() {
  const org = useOrgSlug();
  const { steps, prefs, loaded } = useOnboarding();
  const open = steps.filter(isOpen).length;
  if (!loaded || prefs.dismissed || open === 0) return null;
  return (
    <section className={`${styles.banner} ${styles.touch}`} aria-label="Get Launchpad ready">
      <span className={styles.bannerText}>
        <strong>Get Launchpad ready.</strong> {open === 1 ? '1 step is' : `${open} steps are`} still to do.
      </span>
      <Link to={`/${org}/settings/setup`} className={`${ui.btn} ${ui.primary}`}>
        Open the checklist
      </Link>
      <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={() => prefs.setDismissed(true)}>
        Dismiss
      </button>
    </section>
  );
}
