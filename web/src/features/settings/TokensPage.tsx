import { useState } from 'react';
import { useMembers, useRevokeToken, useTokens } from '../../api/queries';
import type { ApiToken, CreatedToken } from '../../api/types';
import { useAuthedSession } from '../auth/session';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { TokenForm, type Owner } from './TokenForm';
import { TokenSecretDialog } from './TokenSecret';
import { accessText, day, isLive } from './tokenText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';

/** The signed-in person first, then the agents: the people a token can be made for (decisions-T11 #4). */
export function useOwners(): Owner[] {
  const { user } = useAuthedSession();
  const members = useMembers();
  const agents = (members.data ?? []).filter((m) => m.isAgent);
  return [
    { username: user.username, name: user.name, isAgent: false },
    ...agents.map((m) => ({ username: m.username, name: m.name, isAgent: true })),
  ];
}

export function TokensPage() {
  const tokens = useTokens();
  const owners = useOwners();
  const revoke = useRevokeToken();
  // The secret exists here and in the dialog below, in component state only. Done removes it.
  const [issued, setIssued] = useState<CreatedToken | null>(null);
  const [target, setTarget] = useState<ApiToken | null>(null);
  const [revokeError, setRevokeError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);

  const live = (tokens.data ?? []).filter((t) => isLive(t));
  const agentNames = new Set(owners.filter((o) => o.isAgent).map((o) => o.username));

  async function confirmRevoke() {
    if (!target) return;
    setRevokeError(null);
    try {
      await revoke.mutateAsync(target.id);
      setNote(`Token ${target.name} is revoked.`);
      setTarget(null);
    } catch {
      setRevokeError('Could not revoke the token. Try again.');
    }
  }

  return (
    <SettingsPage
      title="API tokens"
      description={
        <>
          A token lets a program use Launchpad as its owner. A token sees the same teams as its owner. The
          list shows no secret. A token made here always expires.
        </>
      }
      wide
    >
      {tokens.isPending ? (
        <SkeletonRows n={3} />
      ) : tokens.isError ? (
        <ErrorState
          error={tokens.error}
          onRetry={() => void tokens.refetch()}
          title="Could not load tokens"
        />
      ) : live.length === 0 ? (
        <EmptyState title="No tokens yet">
          <p>Create the first token with the form below.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <table className={`${styles.table} ${styles.wide}`}>
            <caption className="sr-only">API tokens</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Owner</th>
                <th scope="col">Access</th>
                <th scope="col">Created</th>
                <th scope="col">Last used</th>
                <th scope="col">Expires</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {live.map((t) => (
                <tr key={t.id}>
                  <td>{t.name}</td>
                  <td>
                    {t.user.name}
                    {agentNames.has(t.user.username) && <span className={styles.tag}>Agent</span>}
                  </td>
                  <td>{accessText(t.scopes)}</td>
                  <td>{day(t.createdAt)}</td>
                  <td>{day(t.lastUsedAt)}</td>
                  <td>{day(t.expiresAt)}</td>
                  <td>
                    <button
                      type="button"
                      className={`${ui.btn} ${ui.danger}`}
                      aria-label={`Revoke ${t.name}`}
                      onClick={() => {
                        setRevokeError(null);
                        setNote(null);
                        setTarget(t);
                      }}
                    >
                      Revoke
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}
      <span role="status" className={ui.hint}>
        {note}
      </span>

      <SettingsSection title="Create a token" id="token-form-title">
        <SettingsCard padded>
          <TokenForm
            idPrefix="token"
            owners={owners}
            initialOwner={owners[0]!.username}
            onIssued={(c) => {
              setNote(null);
              setIssued(c);
            }}
          />
        </SettingsCard>
      </SettingsSection>

      {issued && <TokenSecretDialog issued={issued} onDone={() => setIssued(null)} />}
      {target && (
        <Dialog
          title="Revoke this token?"
          onClose={() => setTarget(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setTarget(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={revoke.isPending}
                onClick={() => void confirmRevoke()}
              >
                {revoke.isPending ? 'Revoking…' : 'Revoke token'}
              </button>
            </>
          }
        >
          <p>
            The token <strong>{target.name}</strong> for {target.user.name} stops working at once. You cannot
            undo this.
          </p>
          {revokeError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {revokeError}
            </p>
          )}
        </Dialog>
      )}
    </SettingsPage>
  );
}
