import { useEffect, useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import { keys } from '../../api/keys';
import { useOrgSlug } from '../../api/org';
import { useCreateAgent, useMembers, useTokens } from '../../api/queries';
import { useQueryClient } from '@tanstack/react-query';
import type { CreatedToken, Member } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { TokenForm } from './TokenForm';
import {
  accessBody,
  allTeamsAccess,
  PICK_A_TEAM,
  TeamAccessField,
  teamAccessError,
  type TeamAccessValue,
} from './TeamAccessField';
import { TokenSecretDialog } from './TokenSecret';
import { useOwners } from './TokensPage';
import { day, isLive } from './tokenText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';

// The rules are the server's (openapi AgentInput, decisions-T10 #2). The server stays the judge.
const USERNAME_RE = /^[a-z][a-z0-9_-]{0,38}$/;
const USERNAME_RULE = 'Use 1 to 39 characters: a-z, 0-9, _ and -. Start with a letter.';
const NAME_RULE = 'Enter 1 to 200 characters.';

type Field = 'username' | 'name' | 'teams';

/** The access column (decisions-T20 #33): filled for an org admin only, which this page needs. */
function teamsText(m: Member): string {
  if (m.teamAccess === 'selected') return m.teams?.length ? m.teams.join(', ') : 'No team';
  return m.teamAccess === 'all' ? 'All teams' : '—';
}

export function AgentsPage() {
  const members = useMembers();
  const tokens = useTokens();
  const owners = useOwners();
  const create = useCreateAgent();
  const qc = useQueryClient();
  const org = useOrgSlug();
  const [username, setUsername] = useState('');
  const [name, setName] = useState('');
  const [access, setAccess] = useState<TeamAccessValue>(allTeamsAccess);
  const [errors, setErrors] = useState<Partial<Record<Field, string>>>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [focusField, setFocusField] = useState<Field | null>(null);
  const [made, setMade] = useState<Member | null>(null);
  const [tokenFor, setTokenFor] = useState<Member | null>(null);
  // The secret exists here and in the dialog below, in component state only. Done removes it.
  const [issued, setIssued] = useState<CreatedToken | null>(null);

  // Move focus after the error is on screen (state updates are batched, so the DOM is not ready in the handler).
  useEffect(() => {
    if (!focusField) return;
    const el = document.getElementById(`agent-${focusField}`);
    // The team access is a fieldset: focus its first team box, else its first radio.
    (el instanceof HTMLFieldSetElement
      ? (el.querySelector<HTMLInputElement>('input[type="checkbox"]') ??
        el.querySelector<HTMLInputElement>('input'))
      : el
    )?.focus();
    setFocusField(null);
  }, [focusField]);

  function fail(field: Field, text: string) {
    setErrors({ [field]: text });
    setFocusField(field);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    setMade(null);
    const u = username.trim().toLowerCase();
    const n = name.trim();
    if (!USERNAME_RE.test(u)) return fail('username', USERNAME_RULE);
    if (n.length < 1 || [...n].length > 200) return fail('name', NAME_RULE);
    if (access.teamAccess === 'selected' && access.teams.length === 0) return fail('teams', PICK_A_TEAM);
    setErrors({});
    try {
      const m = await create.mutateAsync({ username: u, name: n, ...accessBody(access) });
      setUsername('');
      setName('');
      setAccess(allTeamsAccess);
      setMade(m);
    } catch (err) {
      // The webhook guard (decisions-T20 #65) is a 409 too; its text names repositories, so a fixed text is shown.
      const teamErr = teamAccessError(err);
      if (teamErr) {
        if (teamErr.gone) {
          // A team was deleted (or hidden) since the list was read: read it again.
          void qc.invalidateQueries({ queryKey: keys.teams(org) });
          setAccess((a) => ({ ...a, teams: [] }));
        }
        fail('teams', teamErr.text);
      } else if (!(err instanceof ApiError)) setBanner('Something went wrong. Try again.');
      else if (err.status === 409) fail('username', 'This username is in use. Use another username.');
      else if (err.status === 400 && err.message.startsWith('username')) fail('username', USERNAME_RULE);
      else if (err.status === 400 && err.message.startsWith('name')) fail('name', NAME_RULE);
      else if (err.status === 403) setBanner('You need admin access to create an agent.');
      else if (err.status === 0)
        setBanner(err.message); // fixed client text: could not reach the server
      else setBanner('Could not create the agent. Try again.');
    }
  }

  const agents = (members.data ?? []).filter((m) => m.isAgent);
  const liveCount = (m: Member) => (tokens.data ?? []).filter((t) => t.user.id === m.id && isLive(t)).length;

  return (
    <SettingsPage
      title="Agents"
      description={
        <>
          An agent is a user for a program, for example the MCP server. It has no email and cannot sign in. It
          works with tokens.
        </>
      }
      wide
    >
      {members.isPending ? (
        <SkeletonRows n={3} />
      ) : members.isError ? (
        <ErrorState
          error={members.error}
          onRetry={() => void members.refetch()}
          title="Could not load agents"
        />
      ) : agents.length === 0 ? (
        <EmptyState title="No agents yet">
          <p>Create the first agent with the form below.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <table className={styles.table}>
            <caption className="sr-only">Agents</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Username</th>
                <th scope="col">Teams</th>
                <th scope="col">Tokens</th>
                <th scope="col">Created</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {agents.map((m) => (
                <tr key={m.id}>
                  <td>{m.name}</td>
                  <td>
                    <code>{m.username}</code>
                  </td>
                  <td>{teamsText(m)}</td>
                  <td>{tokens.isSuccess ? liveCount(m) : '—'}</td>
                  <td>{day(m.createdAt)}</td>
                  <td>
                    <button
                      type="button"
                      className={ui.btn}
                      aria-label={`Create a token for ${m.name}`}
                      onClick={() => setTokenFor(m)}
                    >
                      Create a token
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}

      <SettingsSection title="Create an agent" id="agent-form-title">
        <SettingsCard padded>
          <form onSubmit={(e) => void submit(e)} noValidate aria-labelledby="agent-form-title">
            <div className={styles.pair}>
              <div className={ui.field}>
                <label htmlFor="agent-username">Username</label>
                <input
                  id="agent-username"
                  className={ui.input}
                  value={username}
                  onChange={(e) => setUsername(e.target.value.toLowerCase())}
                  aria-invalid={!!errors.username}
                  aria-describedby={errors.username ? 'agent-username-error' : undefined}
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                />
                {errors.username && (
                  <span id="agent-username-error" className={styles.fieldError} aria-live="polite">
                    {errors.username}
                  </span>
                )}
              </div>
              <div className={ui.field}>
                <label htmlFor="agent-name">Name</label>
                <input
                  id="agent-name"
                  className={ui.input}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  aria-invalid={!!errors.name}
                  aria-describedby={errors.name ? 'agent-name-error' : undefined}
                  autoComplete="off"
                />
                {errors.name && (
                  <span id="agent-name-error" className={styles.fieldError} aria-live="polite">
                    {errors.name}
                  </span>
                )}
              </div>
            </div>
            <TeamAccessField
              idPrefix="agent"
              value={access}
              onChange={(v) => {
                setAccess(v);
                if (errors.teams) setErrors((e) => ({ ...e, teams: undefined }));
              }}
              error={errors.teams}
            />
            {banner && (
              <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {banner}
              </p>
            )}
            <div className={styles.actions}>
              <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={create.isPending}>
                {create.isPending ? 'Creating…' : 'Create agent'}
              </button>
            </div>
            {/* Always in the page, so a screen reader sees the text arrive and reads it. */}
            <div role="status" className={styles.next}>
              {made && (
                <>
                  <span>Agent {made.name} is created.</span>
                  <button type="button" className={ui.btn} onClick={() => setTokenFor(made)}>
                    Create a token for this agent
                  </button>
                </>
              )}
            </div>
          </form>
        </SettingsCard>
      </SettingsSection>

      {tokenFor && (
        <Dialog title={`Create a token for ${tokenFor.name}`} width={480} onClose={() => setTokenFor(null)}>
          <TokenForm
            idPrefix="agent-token"
            // The agent comes from the member we hold, not from the members query, which may not have
            // refetched yet: the form shows the right owner and the agent defaults at once.
            owners={[owners[0]!, { username: tokenFor.username, name: tokenFor.name, isAgent: true }]}
            initialOwner={tokenFor.username}
            lockOwner
            autoFocus
            onCancel={() => setTokenFor(null)}
            onIssued={(c) => {
              setTokenFor(null);
              setMade(null);
              setIssued(c);
            }}
          />
        </Dialog>
      )}
      {issued && <TokenSecretDialog issued={issued} onDone={() => setIssued(null)} />}
    </SettingsPage>
  );
}
