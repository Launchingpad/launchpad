import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useGitIntegrations } from '../../../api/gitIntegrations';
import { useOrgSlug } from '../../../api/org';
import { EmptyState, SkeletonRows } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsPage } from '../SettingsParts';
import { ConnectDialog } from './ConnectDialog';
import { StatusChip, touchClass } from './gitParts';
import { lastDeliveryText } from './deliveryText';
import { attentionText, modeLabel, providerLabel } from './gitText';
import styles from './git.module.css';
import ui from '../../../styles/ui.module.css';

/** Settings, Integrations (LAUN-9, decisions-T17 #46): one row per git connection. */
export function IntegrationsPage() {
  const org = useOrgSlug();
  const list = useGitIntegrations();
  const [connecting, setConnecting] = useState(false);

  return (
    <SettingsPage
      title="Integrations"
      description="Connect GitLab, GitHub or Bitbucket. A merge request or pull request then moves the ticket it names, as the Git workflow page sets for each team. The list shows no token and no secret."
      wide
    >
      <div className={touchClass}>
        <div className={styles.head}>
          <span />
          <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={() => setConnecting(true)}>
            Connect
          </button>
        </div>
        {list.isPending ? (
          <SkeletonRows n={3} />
        ) : list.isError ? (
          <FixedError
            error={list.error}
            onRetry={() => void list.refetch()}
            title="Could not load the connections"
          />
        ) : list.data.length === 0 ? (
          <EmptyState title="No git connections yet">
            <p>Press Connect to add the first one.</p>
          </EmptyState>
        ) : (
          <SettingsCard>
            <ul className={styles.list} aria-label="Git connections">
              {list.data.map((i) => {
                const why = attentionText(i.status === 'needs_attention' ? i.attentionCode : null);
                return (
                  <li key={i.id}>
                    <Link to={`/${org}/settings/integrations/${i.id}`} className={styles.item}>
                      <span className={styles.name}>
                        {providerLabel(i.provider)}: {i.name}
                      </span>
                      <StatusChip integration={i} />
                      <span className={styles.meta}>
                        {i.baseUrl} · {modeLabel(i.mode)} ·{' '}
                        {i.repoCount === 1 ? '1 repository' : `${i.repoCount} repositories`}
                        {i.secretPending ? ' · new secret waiting' : ''}
                        {` · Last delivery: ${lastDeliveryText(i.lastDelivery)}`}
                        {why ? ` · ${why.title}` : ''}
                      </span>
                    </Link>
                  </li>
                );
              })}
            </ul>
          </SettingsCard>
        )}
      </div>
      {connecting && <ConnectDialog onClose={() => setConnecting(false)} />}
    </SettingsPage>
  );
}
