import { useEffect, useId, useRef, useState } from 'react';
import { ApiError } from '../../api/errors';
import {
  useGitAccounts,
  useGitIdentities,
  useRemoveGitAccount,
  useRemoveGitIdentity,
  useSaveGitAccount,
} from '../../api/queries';
import type { GitAccount, GitIdentity } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, SkeletonRows } from '../../ui/States';
import { useIsOrgAdmin } from './access';
import {
  accountSaveText,
  accountsLoadText,
  connectionTitle,
  handleHint,
  hostLabel,
  identityRemoveText,
} from './connectedAccountsText';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import styles from './connectedAccounts.module.css';
import settings from './settings.module.css';
import ui from '../../styles/ui.module.css';

// LAUN-199 (docs/internal/decisions-T34-connected-accounts.md). Settings > Connected accounts, for every
// person (an agent gets 403). The person types the handle they have on each git connection of the org. The
// link is a claim, shown on the Changes block of an issue; it gives no access. An org admin also sees every
// link and can remove one.

function AccountRow({ account }: { account: GitAccount }) {
  const save = useSaveGitAccount();
  const remove = useRemoveGitAccount();
  const inputId = useId();
  const hintId = useId();
  const inputRef = useRef<HTMLInputElement>(null);
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const value = draft ?? account.handle ?? '';
  const dirty = value.trim() !== (account.handle ?? '');
  const busy = save.isPending || remove.isPending;

  async function onSave(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setNote(null);
    try {
      await save.mutateAsync({ integrationId: account.integrationId, handle: value });
      setDraft(null);
      setNote('Saved.');
    } catch (err) {
      setError(accountSaveText(err, account.provider));
    }
  }

  async function onRemove() {
    setError(null);
    setNote(null);
    try {
      await remove.mutateAsync(account.integrationId);
      setDraft(null);
      setNote('Removed.');
      // The Remove button is gone now; keep the keyboard in this row.
      inputRef.current?.focus();
    } catch (err) {
      setError(accountSaveText(err, account.provider));
    }
  }

  return (
    <li className={styles.item}>
      <div>
        <div className={styles.title}>{connectionTitle(account)}</div>
        {!account.enabled && <div className={styles.hint}>This connection is turned off.</div>}
      </div>
      <form className={styles.form} onSubmit={(e) => void onSave(e)} aria-label={connectionTitle(account)}>
        <div className={`${ui.field} ${styles.field}`} style={{ marginBottom: 0 }}>
          <label htmlFor={inputId}>Your handle</label>
          <input
            id={inputId}
            ref={inputRef}
            aria-label={`Your handle on ${connectionTitle(account)}`}
            className={ui.input}
            value={value}
            maxLength={101}
            autoComplete="off"
            autoCapitalize="off"
            spellCheck={false}
            aria-describedby={hintId}
            aria-invalid={error ? true : undefined}
            onChange={(e) => {
              setDraft(e.target.value);
              setError(null);
              setNote(null);
            }}
          />
        </div>
        <button
          type="submit"
          className={`${ui.btn} ${ui.primary}`}
          disabled={busy || !dirty || !value.trim() || !account.enabled}
        >
          {save.isPending ? 'Saving…' : account.handle ? 'Change' : 'Link'}
        </button>
        {account.handle && (
          <button type="button" className={ui.btn} disabled={busy} onClick={() => void onRemove()}>
            Remove
          </button>
        )}
      </form>
      <div id={hintId} className={styles.hint}>
        {handleHint(account.provider)}
      </div>
      <div className={styles.status} role="status">
        {note}
      </div>
      {error && (
        <p role="alert" className={styles.err}>
          {error}
        </p>
      )}
    </li>
  );
}

function AdminLinks() {
  const links = useGitIdentities(true);
  const remove = useRemoveGitIdentity();
  const [target, setTarget] = useState<GitIdentity | null>(null);
  const [error, setError] = useState<string | null>(null);
  const sectionRef = useRef<HTMLDivElement>(null);
  // After a removal focus goes to the next row's Remove button, else the previous, else the section heading.
  const pendingFocus = useRef<{ removed: string; next: string | null } | null>(null);

  useEffect(() => {
    const p = pendingFocus.current;
    if (!p || target || !links.data || links.data.some((l) => l.id === p.removed)) return;
    pendingFocus.current = null;
    const btn = p.next ? sectionRef.current?.querySelector<HTMLElement>(`[data-link="${p.next}"]`) : null;
    if (btn) {
      btn.focus();
      return;
    }
    const h = sectionRef.current?.closest('section')?.querySelector<HTMLElement>('h2');
    if (h) {
      h.tabIndex = -1;
      h.focus();
    }
  }, [links.data, target]);

  async function doRemove(l: GitIdentity) {
    setError(null);
    const rows = links.data ?? [];
    const i = rows.findIndex((r) => r.id === l.id);
    pendingFocus.current = { removed: l.id, next: (rows[i + 1] ?? rows[i - 1])?.id ?? null };
    try {
      await remove.mutateAsync(l.id);
      setTarget(null);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) setTarget(null);
      else pendingFocus.current = null;
      setError(identityRemoveText(e));
    }
  }

  return (
    <SettingsSection
      title="Links in this organisation"
      description="Every member's handle. A member types their own handle, so it is a claim and not a proof. Remove a link that is wrong or that blocks the real owner of a handle."
    >
      <div ref={sectionRef}>
        {links.isPending ? (
          <SkeletonRows n={2} />
        ) : links.isError ? (
          <FixedError
            error={links.error}
            onRetry={() => void links.refetch()}
            title="Could not load the links"
          />
        ) : links.data.length === 0 ? (
          <EmptyState title="No links yet">
            <p>No member has linked a handle.</p>
          </EmptyState>
        ) : (
          <SettingsCard>
            <div className={styles.linkScroll}>
              <table className={`${settings.table} ${styles.linkTable}`}>
                <caption className="sr-only">Linked git handles of the members</caption>
                <thead>
                  <tr>
                    <th scope="col">Member</th>
                    <th scope="col">Connection</th>
                    <th scope="col">Handle</th>
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {links.data.map((l) => (
                    <tr key={l.id}>
                      <td data-label="Member">{l.user.name}</td>
                      <td data-label="Connection">{l.integrationName}</td>
                      <td data-label="Handle">{l.handle}</td>
                      <td>
                        <button
                          type="button"
                          data-link={l.id}
                          className={`${ui.btn} ${ui.danger}`}
                          aria-label={`Remove the link of ${l.user.name} on ${l.integrationName}`}
                          onClick={() => {
                            setError(null);
                            setTarget(l);
                          }}
                        >
                          Remove
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </SettingsCard>
        )}
        {error && !target && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {error}
          </p>
        )}
        {target && (
          <Dialog
            title="Remove this link?"
            onClose={() => setTarget(null)}
            footer={
              <>
                <button type="button" className={ui.btn} data-autofocus onClick={() => setTarget(null)}>
                  Cancel
                </button>
                <button
                  type="button"
                  className={`${ui.btn} ${ui.danger}`}
                  disabled={remove.isPending}
                  onClick={() => void doRemove(target)}
                >
                  {remove.isPending ? 'Removing…' : 'Remove'}
                </button>
              </>
            }
          >
            <p>
              {target.user.name} will no longer show as the author of change requests from this{' '}
              {hostLabel(target.provider)} handle. The member can link a handle again. The member keeps all
              access.
            </p>
            {error && (
              <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {error}
              </p>
            )}
          </Dialog>
        )}
      </div>
    </SettingsSection>
  );
}

export function ConnectedAccountsPage() {
  const accounts = useGitAccounts();
  const isAdmin = useIsOrgAdmin();
  const forbidden = accounts.error instanceof ApiError && accounts.error.status === 403;

  return (
    <SettingsPage
      title="Connected accounts"
      description="Tell Launchpad who you are on each git host. A change request from your handle then shows your name. A link is for display only: it gives you no access and takes none away. No password or token is stored. This link is a claim you make. It only decides how change requests are shown. It does not prove you own the account."
      wide
    >
      {accounts.isPending ? (
        <SkeletonRows n={2} />
      ) : forbidden ? (
        <EmptyState title="No connected accounts for this account">
          <p>{accountsLoadText(accounts.error)}</p>
        </EmptyState>
      ) : accounts.isError ? (
        <FixedError
          error={accounts.error}
          onRetry={() => void accounts.refetch()}
          title="Could not load connected accounts"
        />
      ) : accounts.data.length === 0 ? (
        <EmptyState title="No git connections">
          <p>This organisation has no git connection yet. An org admin adds one under Integrations.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <ul className={styles.list} aria-label="Git connections">
            {accounts.data.map((a) => (
              <AccountRow key={a.integrationId} account={a} />
            ))}
          </ul>
        </SettingsCard>
      )}
      {isAdmin && <AdminLinks />}
    </SettingsPage>
  );
}
