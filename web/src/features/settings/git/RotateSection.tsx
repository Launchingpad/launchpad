import { useEffect, useState } from 'react';
import { useGitActions, useRotateGitSecret } from '../../../api/gitIntegrations';
import type { GitIntegration, GitRepo } from '../../../api/types';
import { Dialog } from '../../../ui/Dialog';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { SecretOnceDialog, touchClass } from './gitParts';
import { gitErrorText, hookText } from './gitText';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

type Confirm = 'discard' | 'force' | null;

/**
 * Rotate the webhook secret (decisions-T17 #11, LAUN-86). Token mode: Launchpad moves every hook, and
 * `failedRepos` can be retried. Webhook only: the new secret is shown once, the admin updates the host, then
 * commits. The pending state comes from the integration (`secretPending`) and the repos' `hookState`, so a
 * reload shows it again. The new secret is held only while its dialog is open.
 */
export function RotateSection({
  integration,
  repos,
  reposKnown = true,
  startSignal = 0,
}: {
  integration: GitIntegration;
  repos: GitRepo[];
  /** False while the repo list is loading or failed: which hooks moved is then unknown. */
  reposKnown?: boolean;
  /** LAUN-104: a count the page raises to start a rotation from elsewhere (the token dialog). 0 starts nothing. */
  startSignal?: number;
}) {
  const rotate = useRotateGitSecret();
  const { commit, discard } = useGitActions(integration.id);
  const tokenMode = integration.mode === 'token';
  const [shown, setShown] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<Confirm>(null);

  // The server counts every hook that is not `active` (tracker/git_rotation.go), so the page does the same.
  const notMoved = repos.filter((r) => r.hookState !== 'active');

  async function start() {
    setBusy(true);
    setNote(null);
    setError(null);
    try {
      const r = await rotate(integration.id);
      if (r.webhookSecret) setShown(r.webhookSecret);
      else if (r.committed) setNote('The secret is rotated. Every hook uses the new secret.');
      else if (r.failedRepos.length === 0)
        setNote(
          'The new secret is stored but not committed yet. Both secrets work. Press Commit when every hook has it.',
        );
      else
        setNote(
          `${r.failedRepos.length === 1 ? '1 hook' : `${r.failedRepos.length} hooks`} did not take the new secret yet. Both secrets work until you commit. Try the hooks again, or commit anyway.`,
        );
    } catch (err) {
      setError(gitErrorText(err, 'rotate'));
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (startSignal > 0) void start();
    // Only a new signal starts a rotation; `start` is a fresh function each render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [startSignal]);

  async function doCommit(force: boolean) {
    setError(null);
    setNote(null);
    setConfirm(null);
    try {
      await commit.mutateAsync(force);
      setNote('The new secret is now the only secret.');
    } catch (err) {
      setError(gitErrorText(err, 'rotate'));
    }
  }

  async function doDiscard() {
    setError(null);
    setNote(null);
    setConfirm(null);
    try {
      await discard.mutateAsync();
      setNote('The new secret is discarded. The old secret still works.');
    } catch (err) {
      setError(gitErrorText(err, 'rotate'));
    }
  }

  const pending = integration.secretPending;
  const working = busy || commit.isPending || discard.isPending;

  return (
    <SettingsSection
      title="Webhook secret"
      description="Rotate the secret from time to time, or when it may have leaked. The old secret keeps working until you commit, so no delivery is lost."
    >
      <SettingsCard padded>
        {pending ? (
          tokenMode ? (
            <>
              <p>
                <strong>A new secret is waiting.</strong> Both secrets work.{' '}
                {!reposKnown
                  ? 'The list of hooks did not load, so Launchpad cannot say which have moved. Reload the page.'
                  : notMoved.length > 0
                    ? 'These hooks are not on the new secret yet:'
                    : 'Every hook is on the new secret. Press Commit to finish.'}
              </p>
              {notMoved.length > 0 && (
                <ul>
                  {notMoved.map((r) => (
                    <li key={r.id}>
                      {r.repoPath}: {hookText(r)}
                    </li>
                  ))}
                </ul>
              )}
            </>
          ) : (
            <p>
              <strong>A new secret is waiting.</strong> Both secrets work. Put the new secret in the hook on
              the host, then press Commit. If you lost it, make another new secret: the first one is dropped.
            </p>
          )
        ) : (
          <p>
            {tokenMode
              ? 'Launchpad makes a new secret and puts it in every hook.'
              : 'Launchpad makes a new secret and shows it once. You put it in the hook on the host, then commit.'}
          </p>
        )}
        <div className={settings.actions}>
          {pending && (
            <button
              type="button"
              className={`${ui.btn} ${ui.primary}`}
              disabled={working || (tokenMode && (notMoved.length > 0 || !reposKnown))}
              onClick={() => void doCommit(false)}
            >
              Commit
            </button>
          )}
          <button type="button" className={ui.btn} disabled={working} onClick={() => void start()}>
            {busy
              ? 'Rotating…'
              : pending
                ? tokenMode
                  ? 'Try the hooks again'
                  : 'Make another new secret'
                : 'Rotate secret'}
          </button>
          {pending && tokenMode && notMoved.length > 0 && (
            <button type="button" className={ui.btn} disabled={working} onClick={() => setConfirm('force')}>
              Commit anyway
            </button>
          )}
          {pending && (
            <button
              type="button"
              className={`${ui.btn} ${ui.danger}`}
              disabled={working}
              onClick={() => setConfirm('discard')}
            >
              Discard new secret
            </button>
          )}
        </div>
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
      </SettingsCard>

      {shown && (
        <SecretOnceDialog
          title="Copy the new secret"
          secret={shown}
          confirmLabel="I saved the new secret."
          onDone={() => {
            setShown(null);
            setNote('Put the new secret in the hook on the host. Then press Commit.');
          }}
        >
          <p className={ui.hint}>
            The old secret still works. After you update the host, press Commit on the page. Then the old
            secret stops.
          </p>
        </SecretOnceDialog>
      )}
      {confirm === 'discard' && (
        <Dialog
          className={touchClass}
          title="Discard the new secret?"
          onClose={() => setConfirm(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setConfirm(null)}>
                Cancel
              </button>
              <button type="button" className={`${ui.btn} ${ui.danger}`} onClick={() => void doDiscard()}>
                Discard
              </button>
            </>
          }
        >
          <p>
            The old secret stays. A hook that already has the new secret gets refused (401) until you refresh
            it or rotate again.
          </p>
        </Dialog>
      )}
      {confirm === 'force' && (
        <Dialog
          className={touchClass}
          title="Commit anyway?"
          onClose={() => setConfirm(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setConfirm(null)}>
                Cancel
              </button>
              <button type="button" className={`${ui.btn} ${ui.danger}`} onClick={() => void doCommit(true)}>
                Commit anyway
              </button>
            </>
          }
        >
          <p>
            The old secret stops working. The {notMoved.length === 1 ? 'hook' : 'hooks'} that did not move
            will be refused (401) until you fix {notMoved.length === 1 ? 'it' : 'them'}.
          </p>
        </Dialog>
      )}
    </SettingsSection>
  );
}
