import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ApiError, cleanServerText } from '../../../api/errors';
import {
  useDeleteGitIntegration,
  useGitActions,
  useGitIntegration,
  useGitRepos,
} from '../../../api/gitIntegrations';
import { useOrgSlug } from '../../../api/org';
import { AlertStack } from '../../../ui/AlertStack';
import { Dialog } from '../../../ui/Dialog';
import { SkeletonRows } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsPage, SettingsRow, SettingsSection } from '../SettingsParts';
import { ReplaceTokenDialog } from './ReplaceTokenDialog';
import { RepoSection } from './RepoSection';
import { DeliveriesSection } from './DeliveriesSection';
import { RotateSection } from './RotateSection';
import { StatusChip, WebhookUrl, touchClass } from './gitParts';
import { lastDeliveryText } from './deliveryText';
import {
  DISABLE_SENTENCE,
  attentionText,
  gitErrorText,
  modeLabel,
  providerInfo,
  providerLabel,
} from './gitText';
import styles from './git.module.css';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

/** Settings, Integrations, one connection (LAUN-9, decisions-T17 #47). */
export function IntegrationPage() {
  const { integration: id = '' } = useParams();
  const org = useOrgSlug();
  const q = useGitIntegration(id);
  const repos = useGitRepos(id);
  const actions = useGitActions(id);
  const remove = useDeleteGitIntegration();
  const navigate = useNavigate();
  const [replacing, setReplacing] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [removeError, setRemoveError] = useState<string | null>(null);
  const [rotateSignal, setRotateSignal] = useState(0);
  const [confirmOff, setConfirmOff] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const back = (
    <Link to={`/${org}/settings/integrations`} className={styles.back}>
      Back to integrations
    </Link>
  );

  if (q.isPending)
    return (
      <SettingsPage title="Integration" wide>
        <div className={touchClass}>
          {back}
          <SkeletonRows n={4} />
        </div>
      </SettingsPage>
    );
  if (q.isError) {
    const gone = q.error instanceof ApiError && q.error.status === 404;
    return (
      <SettingsPage title={gone ? 'Connection not found' : 'Integration'} wide>
        <div className={touchClass}>
          {back}
          {gone ? (
            <p className={ui.hint}>This connection does not exist, or it was removed.</p>
          ) : (
            <FixedError
              error={q.error}
              onRetry={() => void q.refetch()}
              title="Could not load the connection"
            />
          )}
        </div>
      </SettingsPage>
    );
  }

  const i = q.data;
  const info = providerInfo(i.provider);
  const why = i.status === 'needs_attention' ? attentionText(i.attentionCode) : null;

  async function setEnabled(enabled: boolean) {
    setError(null);
    setNote(null);
    setConfirmOff(false);
    try {
      await actions.setEnabled.mutateAsync(enabled);
      setNote(enabled ? 'The connection is on.' : 'The connection is off. Deliveries are refused.');
    } catch (err) {
      setError(gitErrorText(err));
    }
  }

  async function removeConnection() {
    setRemoveError(null);
    setRemoving(true);
    try {
      await remove(id);
      setConfirmRemove(false);
      navigate(`/${org}/settings/integrations`);
    } catch (err) {
      setRemoveError(gitErrorText(err, 'remove'));
    } finally {
      setRemoving(false);
    }
  }

  async function check() {
    setError(null);
    setNote(null);
    try {
      const r = await actions.check.mutateAsync();
      const login = cleanServerText(r.login);
      setNote(login ? `The token works. It belongs to ${login}.` : 'The token works.');
    } catch (err) {
      setError(gitErrorText(err, 'check', i.provider));
    }
  }

  return (
    <SettingsPage title={`${providerLabel(i.provider)}: ${i.name}`} wide>
      <div className={touchClass}>
        {back}
        <div className={styles.head}>
          <span>
            <StatusChip integration={i} /> <span className={ui.hint}>{i.baseUrl}</span>
          </span>
        </div>

        <AlertStack>
          {why && (
            <div role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              <strong>{why.title}</strong> {why.fix}
            </div>
          )}
          {i.status === 'disabled' && (
            <div role="status" className={ui.banner}>
              This connection is off. The host gets 401 for every delivery.
            </div>
          )}
          {note && (
            <p role="status" className={ui.hint}>
              {note}
            </p>
          )}
          {error && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {error}
            </p>
          )}
        </AlertStack>

        <SettingsSection title="Connection">
          <SettingsCard>
            <SettingsRow
              label="Mode"
              description={
                i.mode === 'token'
                  ? 'Launchpad makes the hooks with a token.'
                  : 'You make the hooks on the host.'
              }
            >
              {modeLabel(i.mode)}
            </SettingsRow>
            <SettingsRow
              label="Token"
              description={
                i.mode === 'token'
                  ? 'Sealed on the server. It is never shown.'
                  : 'Add a token to let Launchpad make the hooks.'
              }
            >
              <div className={styles.slot}>
                {i.mode === 'token' && (
                  <button
                    type="button"
                    className={ui.btn}
                    disabled={actions.check.isPending}
                    onClick={() => void check()}
                  >
                    {actions.check.isPending ? 'Checking…' : 'Check token'}
                  </button>
                )}
                <button type="button" className={ui.btn} onClick={() => setReplacing(true)}>
                  {i.mode === 'token' ? 'Replace token' : 'Add a token'}
                </button>
              </div>
            </SettingsRow>
            <SettingsRow
              label="Last delivery"
              description="The newest webhook request that matched this connection."
            >
              {lastDeliveryText(i.lastDelivery)}
            </SettingsRow>
            <SettingsRow label={i.enabled ? 'Turned on' : 'Turned off'} description={DISABLE_SENTENCE}>
              {i.enabled ? (
                <button
                  type="button"
                  className={`${ui.btn} ${ui.danger}`}
                  onClick={() => setConfirmOff(true)}
                >
                  Disable
                </button>
              ) : (
                <button
                  type="button"
                  className={ui.btn}
                  disabled={actions.setEnabled.isPending}
                  onClick={() => void setEnabled(true)}
                >
                  Enable
                </button>
              )}
            </SettingsRow>
            <SettingsRow
              label="Remove connection"
              description="Deletes the connection, its secret and its delivery history. Mapped repositories must be unmapped first."
            >
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                onClick={() => setConfirmRemove(true)}
              >
                Remove connection
              </button>
            </SettingsRow>
          </SettingsCard>
        </SettingsSection>

        {i.mode === 'webhook_only' && info && (
          <SettingsSection
            title="Hook on the host"
            description="Add this webhook to each mapped repository on the host. The secret is not shown again: rotate it for a new one."
          >
            <SettingsCard padded>
              <WebhookUrl url={i.webhookUrl} />
              <ul className={styles.hookList}>
                {info.hookHelp.map((h) => (
                  <li key={h}>{h}</li>
                ))}
              </ul>
            </SettingsCard>
          </SettingsSection>
        )}

        {repos.isPending ? (
          <SkeletonRows n={2} />
        ) : repos.isError ? (
          <FixedError
            error={repos.error}
            onRetry={() => void repos.refetch()}
            title="Could not load the repositories"
          />
        ) : (
          <RepoSection integration={i} repos={repos.data} />
        )}
        {/* Rotation does not wait for the repo list: a pending or unreadable secret must stay reachable. */}
        <RotateSection
          integration={i}
          repos={repos.data ?? []}
          reposKnown={repos.isSuccess}
          startSignal={rotateSignal}
        />
        <DeliveriesSection integration={i} />

        {replacing && (
          <ReplaceTokenDialog
            integration={i}
            onClose={() => setReplacing(false)}
            onRotate={() => {
              setReplacing(false);
              setRotateSignal((n) => n + 1);
            }}
            onDone={() => {
              setReplacing(false);
              setNote('The token is saved.');
              void check();
            }}
          />
        )}
        {confirmRemove && (
          <Dialog
            className={touchClass}
            title={`Remove ${providerLabel(i.provider)}: ${i.name}?`}
            onClose={() => !removing && setConfirmRemove(false)}
            footer={
              <>
                <button
                  type="button"
                  className={ui.btn}
                  disabled={removing}
                  onClick={() => setConfirmRemove(false)}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  className={`${ui.btn} ${ui.danger}`}
                  disabled={removing}
                  onClick={() => void removeConnection()}
                >
                  {removing ? 'Removing…' : 'Remove connection'}
                </button>
              </>
            }
          >
            <p>
              This removes the connection to <strong>{i.baseUrl}</strong>, its token, its webhook secret, its
              delivery history and the status of its merge requests on tickets. Tickets and their links stay.
            </p>
            <p className={settings.noteText}>
              Mapped repositories must be unmapped first. In token mode, unmapping also removes the hooks on
              the host. Hooks that you made yourself stay on the host: delete them there.
            </p>
            {removeError && (
              <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {removeError}
              </p>
            )}
          </Dialog>
        )}
        {confirmOff && (
          <Dialog
            className={touchClass}
            title="Disable this connection?"
            onClose={() => setConfirmOff(false)}
            footer={
              <>
                <button type="button" className={ui.btn} onClick={() => setConfirmOff(false)}>
                  Cancel
                </button>
                <button
                  type="button"
                  className={`${ui.btn} ${ui.danger}`}
                  onClick={() => void setEnabled(false)}
                >
                  Disable
                </button>
              </>
            }
          >
            <p>{DISABLE_SENTENCE}</p>
            <p className={settings.noteText}>You can turn it on again at any time.</p>
          </Dialog>
        )}
      </div>
    </SettingsPage>
  );
}
