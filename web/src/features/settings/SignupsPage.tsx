import { useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { keys } from '../../api/keys';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import {
  useApproveSignup,
  usePlatformSettings,
  usePurgeUnconfirmed,
  useRejectSignup,
  membersQuery,
  useSignups,
  useTeams,
} from '../../api/queries';
import { useAuthedSession } from '../auth/session';
import {
  accessBody,
  allTeamsAccess,
  PICK_A_TEAM,
  TeamAccessField,
  teamAccessError,
  type TeamAccessValue,
} from './TeamAccessField';
import type { SignupRequest } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, SkeletonRows } from '../../ui/States';
import { FixedError } from './FixedError';
import { useIsPlatformAdmin } from './access';
import { age, approveText, isModeRefusal, purgeText, rejectText } from './joinText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';

const REASON_MAX = 1000;

/**
 * Settings, Sign-ups (decisions-T12 #5 to #15). Org admins only. The server judges every call;
 * its texts never reach the screen. The list holds no confirm code or hash.
 */
export function SignupsPage() {
  const org = useOrgSlug();
  const isPlatformAdmin = useIsPlatformAdmin();
  const settings = usePlatformSettings(isPlatformAdmin);
  const signups = useSignups();
  const approve = useApproveSignup();
  const reject = useRejectSignup();
  const purge = usePurgeUnconfirmed();
  const teams = useTeams();
  const qc = useQueryClient();
  const { user } = useAuthedSession();

  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [refused, setRefused] = useState(false);
  const [working, setWorking] = useState<string | null>(null);
  // The approval dialog (decisions-T20 #29, #75): the team access is chosen there, once.
  const [approving, setApproving] = useState<SignupRequest | null>(null);
  const [access, setAccess] = useState<TeamAccessValue>(allTeamsAccess);
  const [accessError, setAccessError] = useState<string | undefined>(undefined);
  const busy = useRef(false); // one approval at a time, also for a fast double click
  const [target, setTarget] = useState<SignupRequest | null>(null);
  const [reason, setReason] = useState('');
  const [block, setBlock] = useState(false);
  const [rejectError, setRejectError] = useState<string | null>(null);
  const [purgeOpen, setPurgeOpen] = useState(false);
  const [purgeError, setPurgeError] = useState<string | null>(null);

  const mode = settings.data?.signupMode;
  const notApproval = (mode !== undefined && mode !== 'approval') || refused;

  function openApprove(r: SignupRequest) {
    setError(null);
    setNote(null);
    setAccess(allTeamsAccess);
    setAccessError(undefined);
    setApproving(r);
  }

  /** The member's access as text, from a fresh member list; null when it cannot be read. */
  async function realAccess(userId: string | null): Promise<string | null> {
    if (!userId) return null;
    try {
      const list = await qc.fetchQuery({ ...membersQuery(org), staleTime: 0 });
      const m = list.find((x) => x.id === userId);
      if (!m || m.teamAccess === null) return null;
      if (m.teamAccess === 'all') return 'all teams';
      return m.teams?.length ? `these teams: ${m.teams.join(', ')}` : 'no team';
    } catch {
      return null;
    }
  }

  async function doApprove() {
    const r = approving;
    if (!r || busy.current) return;
    if (access.teamAccess === 'selected' && access.teams.length === 0) {
      setAccessError(PICK_A_TEAM);
      document
        .getElementById('approve-teams')
        ?.querySelector<HTMLInputElement>('input[type="checkbox"]')
        ?.focus();
      return;
    }
    busy.current = true;
    setAccessError(undefined);
    setWorking(r.id);
    const chosen = access;
    try {
      const done = await approve.mutateAsync({ id: r.id, body: accessBody(chosen) });
      setApproving(null);
      // A request approved before (another admin, this admin in another tab, a retry after a lost answer) answers
      // 200 and keeps its FIRST access, whatever this body asked (LAUN-226). So the note names the access the member
      // really has, read from the member list; when that cannot be read, it names none.
      const real = await realAccess(done.userId);
      const by = done.decidedBy && done.decidedBy !== user.id;
      setNote(
        real === null
          ? `${r.name} is approved. Check the team access on the Members page.`
          : by
            ? `${r.name} was approved already by another admin, with access to ${real}. The access you chose here was not applied.`
            : `${r.name} is approved with access to ${real}. The person can now sign in with an email code.`,
      );
    } catch (err) {
      const teamErr = teamAccessError(err);
      if (teamErr) {
        // The dialog stays open: the choice is what to fix.
        if (teamErr.gone) {
          void qc.invalidateQueries({ queryKey: keys.teams(org) });
          setAccess((a) => ({ ...a, teams: [] }));
        }
        setAccessError(teamErr.text);
      } else {
        setApproving(null);
        if (isModeRefusal(err)) setRefused(true);
        setError(approveText(err));
      }
    } finally {
      busy.current = false;
      setWorking(null);
    }
  }

  function openReject(r: SignupRequest) {
    setError(null);
    setNote(null);
    setRejectError(null);
    setReason('');
    setBlock(false);
    setTarget(r);
  }

  async function doReject() {
    if (!target) return;
    setRejectError(null);
    if ([...reason].length > REASON_MAX) {
      setRejectError('The reason is too long. Use 1000 characters or fewer.');
      return;
    }
    try {
      await reject.mutateAsync({ id: target.id, reason: reason.trim() || undefined, block });
      setNote(
        block
          ? `The request of ${target.name} is rejected. The email address is blocked.`
          : `The request of ${target.name} is rejected.`,
      );
      setTarget(null);
    } catch (err) {
      setRejectError(rejectText(err));
    }
  }

  async function doPurge() {
    setPurgeError(null);
    try {
      const r = await purge.mutateAsync();
      setNote(
        r.deleted === 1 ? 'Removed 1 unconfirmed request.' : `Removed ${r.deleted} unconfirmed requests.`,
      );
      setPurgeOpen(false);
    } catch (err) {
      setPurgeError(purgeText(err));
    }
  }

  const items = signups.data ?? [];

  return (
    <SettingsPage
      title="Sign-ups"
      description={
        <>
          When the sign-up mode is Approval, a person can ask to join on the{' '}
          <Link to="/signup">sign-up page</Link>. The person gets no access until you approve the request.
          When the server can send email, the person must first enter a code from the email. Until then the
          request waits and does not show in this list.
        </>
      }
      wide
    >
      {notApproval && (
        <p role="status" className={ui.banner} data-testid="signups-not-approval">
          <span>
            The sign-up mode is not Approval, so you cannot approve requests now. You can still reject them.{' '}
            {isPlatformAdmin ? (
              <>
                Change the mode on the <Link to={`/${org}/settings/platform`}>Platform page</Link>.
              </>
            ) : (
              'A platform admin can change the mode.'
            )}
          </span>
        </p>
      )}
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}

      {signups.isPending ? (
        <SkeletonRows n={3} />
      ) : signups.isError ? (
        <FixedError
          error={signups.error}
          onRetry={() => void signups.refetch()}
          title="Could not load sign-up requests"
        />
      ) : items.length === 0 ? (
        <EmptyState title="No requests to decide">
          <p>New requests show here.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <table className={`${styles.table} ${styles.wide}`}>
            <caption className="sr-only">Sign-up requests</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col" className={styles.nowrap}>
                  Username
                </th>
                <th scope="col">Email</th>
                <th scope="col">Note</th>
                <th scope="col">Asked</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((r) => (
                <tr key={r.id}>
                  <td>{r.name}</td>
                  <td className={styles.nowrap}>
                    <code>{r.username}</code>
                  </td>
                  <td>
                    <div>{r.email}</div>
                    {!r.emailVerified && <span className={styles.flag}>Email unverified</span>}
                  </td>
                  <td>{r.note ? <p className={styles.noteText}>{r.note}</p> : '—'}</td>
                  <td>{age(r.createdAt)}</td>
                  <td>
                    <div className={styles.rowActions}>
                      <button
                        type="button"
                        className={`${ui.btn} ${ui.primary}`}
                        aria-label={`Approve ${r.name}`}
                        disabled={working !== null}
                        onClick={() => openApprove(r)}
                      >
                        {working === r.id ? 'Approving…' : 'Approve'}
                      </button>
                      <button
                        type="button"
                        className={`${ui.btn} ${ui.danger}`}
                        aria-label={`Reject ${r.name}`}
                        disabled={working !== null}
                        onClick={() => openReject(r)}
                      >
                        Reject
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}
      <p role="status" className={ui.hint} data-testid="signups-note">
        {note}
      </p>
      {items.some((r) => !r.emailVerified) && (
        <p className={ui.hint}>
          “Email unverified” means the server could not send a code, so the person did not prove the address.
          An account that you approve like this must sign in once with an email code. That sign-in ends every
          token and session the account had before.
        </p>
      )}

      <SettingsSection
        title="Remove unconfirmed requests"
        id="purge-title"
        description={
          <>
            Unconfirmed requests wait for the email code. While email works, they are hidden from the list
            above and they expire after 24 hours. At most 1000 can wait. If a flood fills that limit, new
            people get “too many requests”. Remove them here to free the places. Requests with a confirmed
            email stay. Requests that the server took when email was off stay too.
          </>
        }
      >
        <SettingsCard padded>
          <button
            type="button"
            className={`${ui.btn} ${ui.danger}`}
            onClick={() => {
              setPurgeError(null);
              setNote(null);
              setPurgeOpen(true);
            }}
          >
            Remove unconfirmed requests
          </button>
        </SettingsCard>
      </SettingsSection>

      {approving && (
        <Dialog
          title={`Approve ${approving.name}?`}
          onClose={() => {
            if (!busy.current) setApproving(null);
          }}
          footer={
            <>
              <button
                type="button"
                className={ui.btn}
                onClick={() => setApproving(null)}
                disabled={working !== null}
              >
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.primary}`}
                // Wait for the team list, so the choice is made from what exists now.
                disabled={working !== null || teams.isPending}
                onClick={() => void doApprove()}
              >
                {working !== null ? 'Approving…' : teams.isPending ? 'Loading teams…' : 'Approve request'}
              </button>
            </>
          }
        >
          <p>
            {approving.name} ({approving.email}) becomes a member of this organisation. Choose the teams the
            person sees.
          </p>
          <TeamAccessField
            idPrefix="approve"
            value={access}
            onChange={(v) => {
              setAccess(v);
              setAccessError(undefined);
            }}
            error={accessError}
          />
          <p className={ui.hint}>
            The access is set once, by the approval. Approving a request that is approved already changes
            nothing. To change the access later, use the{' '}
            <Link to={`/${org}/settings/members`}>Members page</Link>.
          </p>
        </Dialog>
      )}
      {target && (
        <Dialog
          title={`Reject ${target.name}?`}
          onClose={() => setTarget(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setTarget(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={reject.isPending}
                onClick={() => void doReject()}
              >
                {reject.isPending ? 'Rejecting…' : 'Reject request'}
              </button>
            </>
          }
        >
          <div className={ui.field}>
            <label htmlFor="reject-reason">Reason (optional)</label>
            <textarea
              id="reject-reason"
              className={`${ui.textarea} ${styles.reasonBox}`}
              value={reason}
              maxLength={REASON_MAX}
              onChange={(e) => setReason(e.target.value)}
              aria-describedby="reject-reason-hint"
              data-autofocus
            />
            <span id="reject-reason-hint" className={ui.hint}>
              Only admins see the reason. The person does not get it.
            </span>
          </div>
          <label className={styles.confirm}>
            <input type="checkbox" checked={block} onChange={(e) => setBlock(e.target.checked)} />
            <span>Block this email address ({target.email}). It cannot ask to join again.</span>
          </label>
          {rejectError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {rejectError}
            </p>
          )}
        </Dialog>
      )}
      {purgeOpen && (
        <Dialog
          title="Remove unconfirmed requests?"
          onClose={() => setPurgeOpen(false)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setPurgeOpen(false)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={purge.isPending}
                onClick={() => void doPurge()}
              >
                {purge.isPending ? 'Removing…' : 'Remove requests'}
              </button>
            </>
          }
        >
          <p>
            This deletes every request that waits for its email code. You cannot undo this. The people can ask
            again.
          </p>
          {purgeError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {purgeError}
            </p>
          )}
        </Dialog>
      )}
    </SettingsPage>
  );
}
