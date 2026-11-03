import { useGitDeliveries } from '../../../api/gitIntegrations';
import type { GitIntegration } from '../../../api/types';
import { fullDate } from '../../../lib/time';
import { EmptyState, SkeletonRows } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { deliveryCodeText, resultText } from './deliveryText';
import styles from './git.module.css';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

/**
 * The deliveries ring (LAUN-87, decisions-T17 #9): one summary row per webhook request, newest first, 100 at most.
 * A row holds no payload, secret or caller address, so there is nothing to hide. Every value is a text node.
 * A row of no integration (a wrong secret) is listed on each integration of the provider and says so.
 */
export function DeliveriesSection({ integration }: { integration: Pick<GitIntegration, 'id'> }) {
  const q = useGitDeliveries(integration.id);
  return (
    <SettingsSection
      title="Recent deliveries"
      description="The last 100 webhook requests from the host, newest first. A row shows what Launchpad did with the request. It shows none of the request's content."
    >
      {q.isPending ? (
        <SkeletonRows n={3} />
      ) : q.isError ? (
        <FixedError error={q.error} onRetry={() => void q.refetch()} title="Could not load the deliveries" />
      ) : q.data.length === 0 ? (
        <EmptyState title="No deliveries yet">
          <p>When the host calls the webhook, the request is listed here.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <div className={settings.tableScroll}>
            <table className={settings.table} aria-label="Recent deliveries">
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Event</th>
                  <th scope="col">Repository</th>
                  <th scope="col">Result</th>
                  <th scope="col">HTTP status</th>
                  <th scope="col">Reason</th>
                </tr>
              </thead>
              <tbody>
                {q.data.map((d) => (
                  <tr key={d.id}>
                    <td className={settings.nowrap}>
                      <time dateTime={d.receivedAt}>{fullDate(d.receivedAt)}</time>
                    </td>
                    <td>{d.event || 'Not read'}</td>
                    <td className={styles.mono}>{d.repoKey || 'None'}</td>
                    <td>
                      {resultText(d.result)}
                      {d.integrationId === null && <div className={ui.hint}>no integration matched</div>}
                    </td>
                    <td>{d.httpStatus}</td>
                    <td>{deliveryCodeText(d.errorCode) || 'None'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className={settings.actions}>
            <button type="button" className={ui.btn} onClick={() => void q.refetch()}>
              Reload
            </button>
          </div>
        </SettingsCard>
      )}
    </SettingsSection>
  );
}
