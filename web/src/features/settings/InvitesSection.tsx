import { SettingsCard, SettingsSection } from './SettingsParts';
import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import {
  useCreateInvite,
  useInvites,
  usePlatformSettings,
  useRevokeInvite,
  useTeams,
} from '../../api/queries';
import type { CreatedInvite, Invite, InviteInput, InviteRole } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { SkeletonRows } from '../../ui/States';
import { FixedError } from './FixedError';
import { useIsPlatformAdmin } from './access';
import { InviteLinkDialog, fullLink } from './InviteLinkDialog';
import { TeamChecklist } from './TeamChecklist';
import { age, inviteAccessLabel, inviteCreateText, inviteRevokeText, isModeRefusal } from './joinText';
import { day } from './tokenText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

const STATUS: Record<Invite['status'], string> = {
  pending: 'Pending',
  accepted: 'Accepted',
  revoked: 'Revoked',
  expired: 'Expired',
};

/**
 * Settings, Members: invites (decisions-T12 #16 to #22). Team access (decisions-T20 #75, LAUN-227): "All teams"
 * (preselected) or "Selected teams" with at least one team; hidden for role Admin (an admin invite is always all). The sign-up mode is readable by a platform
 * admin only (`GET /platform/settings`). Other org admins learn it from a `403 signup_unavailable`.
 */
export function InvitesSection() {
  const org = useOrgSlug();
  const isPlatformAdmin = useIsPlatformAdmin();
  const settings = usePlatformSettings(isPlatformAdmin);
  const invites = useInvites();
  const create = useCreateInvite();
  const revoke = useRevokeInvite();

  const [email, setEmail] = useState('');
  const [role, setRole] = useState<InviteRole>('member');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [refused, setRefused] = useState(false); // the server said "closed" to a non-platform admin
  // The invite link exists here and in the dialog below, in component state only. Done removes it.
  const [issued, setIssued] = useState<{ invite: CreatedInvite['invite']; link: string } | null>(null);
  const [target, setTarget] = useState<Invite | null>(null);
  const [revokeError, setRevokeError] = useState<string | null>(null);
  const [focusEmail, setFocusEmail] = useState(false);
  const teams = useTeams();
  const [access, setAccess] = useState<'all' | 'selected'>('all');
  const [picked, setPicked] = useState<string[]>([]);
  const [teamError, setTeamError] = useState<string | null>(null);
  const sending = useRef(false); // a second Enter or click while the create runs is ignored
  const teamsRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!focusEmail) return;
    document.getElementById('invite-email')?.focus();
    setFocusEmail(false);
  }, [focusEmail]);

  const closed = settings.data?.signupMode === 'closed' || refused;

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (sending.current) return;
    setError(null);
    setTeamError(null);
    setNote(null);
    const addr = email.trim();
    if (!EMAIL_RE.test(addr) || addr.length > 254) {
      setError('Enter a valid email address.');
      setFocusEmail(true);
      return;
    }
    const selected = role !== 'admin' && access === 'selected';
    if (selected && !teams.isSuccess) {
      setTeamError('Wait for the team list to load, or choose All teams.');
      return;
    }
    if (selected && picked.length === 0) {
      setTeamError('Pick at least one team, or choose All teams.');
      teamsRef.current?.querySelector<HTMLInputElement>('input[type="checkbox"]')?.focus();
      return;
    }
    // An admin invite is always all (decisions-T20 #30): the access fields are not sent.
    const body: InviteInput =
      role === 'admin'
        ? { email: addr, role }
        : selected
          ? { email: addr, role, teamAccess: 'selected', teams: [...picked].sort() }
          : { email: addr, role, teamAccess: 'all' };
    sending.current = true;
    setBusy(true);
    try {
      const r = await create(body);
      setEmail('');
      setAccess('all');
      setPicked([]);
      const gives =
        r.invite.role !== 'admin' && r.invite.teamAccess === 'selected'
          ? ` The person will see only these teams: ${r.invite.teams.join(', ')}.`
          : '';
      if (r.emailed) {
        setNote(`The invite is sent by email to ${addr}.${gives}`);
      } else if (r.url) {
        setIssued({ invite: r.invite, link: fullLink(r.url) });
      } else {
        setError('The server did not send a link. Revoke the invite and make a new one.');
      }
    } catch (err) {
      if (isModeRefusal(err)) setRefused(true);
      setError(inviteCreateText(err));
    } finally {
      sending.current = false;
      setBusy(false);
    }
  }

  async function confirmRevoke() {
    if (!target) return;
    setRevokeError(null);
    try {
      await revoke.mutateAsync(target.id);
      setNote(`The invite for ${target.email} is revoked.`);
      setTarget(null);
    } catch (err) {
      setRevokeError(inviteRevokeText(err));
    }
  }

  const shown = (invites.data ?? []).filter((i) => i.status !== 'revoked');

  return (
    <SettingsSection
      title="Invites"
      id="invites-title"
      description={
        <>
          An invite lets one email address join. When the server can send email, it sends the link to that
          address, and you never see it. When it cannot, you copy the link one time and send it yourself. A
          link works one time, for 7 days.
        </>
      }
    >
      {closed ? (
        <p role="status" className={`${ui.banner}`} data-testid="invites-closed">
          <span>
            Invites are off while the sign-up mode is Closed.{' '}
            {isPlatformAdmin ? (
              <>
                Change it on the <Link to={`/${org}/settings/platform`}>Platform page</Link>.
              </>
            ) : (
              'A platform admin can change it.'
            )}
          </span>
        </p>
      ) : (
        <SettingsSection title="Invite a person" id="invite-form-title">
          <SettingsCard padded>
            <form onSubmit={(e) => void submit(e)} noValidate aria-labelledby="invite-form-title">
              <div className={styles.pair}>
                <div className={ui.field}>
                  <label htmlFor="invite-email">Email</label>
                  <input
                    id="invite-email"
                    className={ui.input}
                    type="email"
                    inputMode="email"
                    autoComplete="off"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    aria-invalid={!!error}
                    aria-describedby={error ? 'invite-error' : undefined}
                  />
                </div>
                <div className={ui.field}>
                  <label htmlFor="invite-role">Role</label>
                  <select
                    id="invite-role"
                    className={ui.select}
                    value={role}
                    onChange={(e) => setRole(e.target.value as InviteRole)}
                  >
                    <option value="member">Member</option>
                    <option value="admin">Admin</option>
                  </select>
                </div>
              </div>
              {role === 'admin' ? (
                <p className={ui.hint}>An admin always sees every team.</p>
              ) : (
                <>
                  <fieldset className={styles.radios}>
                    <legend>Team access</legend>
                    <label>
                      <input
                        type="radio"
                        name="invite-access"
                        checked={access === 'all'}
                        onChange={() => {
                          setTeamError(null);
                          setAccess('all');
                        }}
                      />
                      <span>All teams</span>
                    </label>
                    <label>
                      <input
                        type="radio"
                        name="invite-access"
                        checked={access === 'selected'}
                        onChange={() => {
                          setTeamError(null);
                          setAccess('selected');
                        }}
                      />
                      <span>Selected teams</span>
                    </label>
                  </fieldset>
                  {access === 'selected' && (
                    <div ref={teamsRef}>
                      {teams.isPending ? (
                        <p role="status" className={ui.hint}>
                          Loading teams…
                        </p>
                      ) : teams.isError ? (
                        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                          Could not load the teams.{' '}
                          <button
                            type="button"
                            className={`${ui.btn} ${ui.ghost}`}
                            onClick={() => void teams.refetch()}
                          >
                            Try again
                          </button>
                        </p>
                      ) : (
                        <TeamChecklist
                          teams={teams.data}
                          picked={picked}
                          onChange={(next) => {
                            setTeamError(null);
                            setPicked(next);
                          }}
                          invalid={!!teamError}
                          describedBy={teamError ? 'invite-teams-error' : undefined}
                        />
                      )}
                      <p className={ui.hint}>The person sees only the teams you tick.</p>
                    </div>
                  )}
                  {teamError && (
                    <p id="invite-teams-error" role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                      {teamError}
                    </p>
                  )}
                </>
              )}
              {error && (
                <p id="invite-error" role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                  {error}
                </p>
              )}
              <div className={styles.actions}>
                <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
                  {busy ? 'Inviting…' : 'Send invite'}
                </button>
              </div>
            </form>
          </SettingsCard>
        </SettingsSection>
      )}
      <p role="status" className={ui.hint} data-testid="invite-note">
        {note}
      </p>

      {invites.isPending ? (
        <SkeletonRows n={2} />
      ) : invites.isError ? (
        <FixedError
          error={invites.error}
          onRetry={() => void invites.refetch()}
          title="Could not load invites"
        />
      ) : shown.length === 0 ? (
        <p className={ui.hint}>No invites yet.</p>
      ) : (
        <SettingsCard>
          <table className={`${styles.table} ${styles.wide}`}>
            <caption className="sr-only">Invites</caption>
            <thead>
              <tr>
                <th scope="col">Email</th>
                <th scope="col">Role</th>
                <th scope="col">Teams</th>
                <th scope="col">Status</th>
                <th scope="col">Sent</th>
                <th scope="col">Expires</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {shown.map((i) => (
                <tr key={i.id}>
                  <td>{i.email}</td>
                  <td>{i.role === 'admin' ? 'Admin' : 'Member'}</td>
                  <td data-testid={`invite-access-${i.id}`}>{inviteAccessLabel(i)}</td>
                  <td>
                    {STATUS[i.status]}
                    {i.status === 'pending' && (
                      <span className={styles.tag}>{i.emailed ? 'Sent by email' : 'Link shown once'}</span>
                    )}
                  </td>
                  <td>{age(i.createdAt)}</td>
                  <td>{day(i.expiresAt)}</td>
                  <td>
                    {i.status === 'pending' && (
                      <button
                        type="button"
                        className={`${ui.btn} ${ui.danger}`}
                        aria-label={`Revoke the invite for ${i.email}`}
                        onClick={() => {
                          setRevokeError(null);
                          setNote(null);
                          setTarget(i);
                        }}
                      >
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}

      {issued && (
        <InviteLinkDialog invite={issued.invite} link={issued.link} onDone={() => setIssued(null)} />
      )}
      {target && (
        <Dialog
          title="Revoke this invite?"
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
                {revoke.isPending ? 'Revoking…' : 'Revoke invite'}
              </button>
            </>
          }
        >
          <p>
            The link for <strong>{target.email}</strong> stops working at once. You can invite the address
            again later.
          </p>
          {revokeError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {revokeError}
            </p>
          )}
        </Dialog>
      )}
    </SettingsSection>
  );
}
