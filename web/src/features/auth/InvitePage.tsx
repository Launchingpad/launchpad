import { useEffect, useRef, useState, type FormEvent, type ReactNode, type RefObject } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { acceptInvite } from '../../api/auth';
import { ApiError } from '../../api/errors';
import { keys } from '../../api/keys';
import { isModeRefusal, waitText } from '../settings/joinText';
import { useAppTitle } from '../../lib/documentTitle';
import styles from './setup.module.css';
import ui from '../../styles/ui.module.css';

// The public invite page, `/invite#<token>` (decisions-T12 #16 to #22). Same token rules as the
// setup page (decisions-T8 #2): the token is read from the URL hash once, removed from the address bar
// and the history entry at once, kept in memory only, and sent only in the POST body. No text from the server
// reaches the screen.

const USERNAME_RE = /^[a-z][a-z0-9_-]{0,38}$/;
const TOKEN_RE = /^[A-Za-z0-9_-]{43}$/;

// A module variable (not state alone) so React StrictMode's second render still finds it after the hash is gone.
let heldToken: string | undefined;

/** Reads the token from the URL hash once, then removes the hash from the address bar and the history entry. */
function takeToken(): string | undefined {
  const raw = window.location.hash.replace(/^#/, '');
  if (raw) {
    heldToken = raw;
    window.history.replaceState(window.history.state, '', window.location.pathname + window.location.search);
  }
  return heldToken;
}

/** Test hook: forget the held token. */
export function forgetInviteToken(): void {
  heldToken = undefined;
}

type Phase = 'form' | 'invalid' | 'closed' | 'joined';
type Field = 'name' | 'username';

export function InvitePage() {
  useAppTitle('Join');
  const qc = useQueryClient();
  const nav = useNavigate();
  const [token, setToken] = useState(takeToken);
  const [phase, setPhase] = useState<Phase>('form');
  const [name, setName] = useState('');
  const [username, setUsername] = useState('');
  const [errors, setErrors] = useState<Partial<Record<Field, string>>>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [orgName, setOrgName] = useState('');
  const [focusField, setFocusField] = useState<Field | null>(null);
  const headingRef = useRef<HTMLHeadingElement>(null);

  // A new link opened in this tab changes only the fragment: the page stays mounted. Take the new token,
  // remove it from the URL again and start over.
  useEffect(() => {
    const onHash = () => {
      if (!window.location.hash.replace(/^#/, '')) return;
      setToken(takeToken());
      setPhase('form');
      setErrors({});
      setBanner(null);
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);
  useEffect(() => forgetInviteToken, []);
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(`invite-${focusField}`)?.focus();
    setFocusField(null);
  }, [focusField]);
  // Only a notice has the heading ref, so the form (with a valid token) keeps its own first-field focus.
  useEffect(() => {
    headingRef.current?.focus();
  }, [phase, token]);

  function fail(field: Field, text: string) {
    setErrors({ [field]: text });
    setFocusField(field);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!token) return;
    setBanner(null);
    const n = name.trim();
    const u = username.trim().toLowerCase();
    // A person with an account leaves both empty. A new person needs both.
    if (n || u) {
      if (!n || [...n].length > 200) return fail('name', 'Enter 1 to 200 characters.');
      if (!USERNAME_RE.test(u))
        return fail('username', 'Use 1 to 39 characters: a-z, 0-9, _ and -. Start with a letter.');
    }
    setErrors({});
    setBusy(true);
    try {
      const r = await acceptInvite({ token, ...(n ? { name: n } : {}), ...(u ? { username: u } : {}) });
      heldToken = undefined;
      setToken(undefined);
      if (r.status === 'joined') {
        setOrgName(r.orgName);
        setPhase('joined');
      } else {
        qc.setQueryData(keys.session, { user: r.user, orgs: r.orgs });
        nav(`/${r.orgs[0]?.slug ?? ''}`, { replace: true });
      }
    } catch (err) {
      handleError(err);
    } finally {
      setBusy(false);
    }
  }

  function handleError(err: unknown) {
    if (!(err instanceof ApiError)) {
      setBanner('Something went wrong. Try again.');
    } else if (err.status === 401) {
      heldToken = undefined;
      setToken(undefined);
      setPhase('invalid');
    } else if (isModeRefusal(err)) {
      heldToken = undefined;
      setPhase('closed');
    } else if (err.status === 409) {
      fail(
        'username',
        'This username is taken, or your account changed. Choose another username and try again.',
      );
    } else if (err.status === 429) {
      setBanner(`Too many tries. Try again in ${waitText(err.retryAfter)}.`);
    } else if (err.status === 400) {
      setBanner('Enter your name and a username. A new account needs both.');
      setFocusField('name');
    } else if (err.status === 0) {
      setBanner('Could not reach the server. Check your connection and try again.');
    } else {
      setBanner('Could not accept the invite. Try again.');
    }
  }

  if (phase === 'joined') {
    return (
      <Notice title="You joined" headingRef={headingRef}>
        <p>
          You are now a member of {orgName || 'the organisation'}. Sign in with an email code to continue.
        </p>
        <Link to="/login">Go to the login page</Link>
      </Notice>
    );
  }
  if (phase === 'closed') {
    return (
      <Notice title="Invites are closed" headingRef={headingRef}>
        <p>This install does not accept invites now. Ask an admin.</p>
      </Notice>
    );
  }
  if (phase === 'invalid') {
    return (
      <Notice title="This invite link does not work" headingRef={headingRef}>
        <p>The link is wrong, expired, used or revoked. Each link works one time, for 7 days.</p>
        <p>Ask an admin for a new invite.</p>
      </Notice>
    );
  }
  if (!token) {
    return (
      <Notice title="You need an invite link" headingRef={headingRef}>
        <p>Open the link from your invite email, or from the admin who invited you.</p>
        <p>
          No invite? You can <Link to="/signup">ask to join</Link> if the install allows it.
        </p>
      </Notice>
    );
  }
  if (!TOKEN_RE.test(token)) {
    return (
      <Notice title="This invite link is not complete" headingRef={headingRef}>
        <p>Copy the whole link, to the last character.</p>
      </Notice>
    );
  }

  return (
    <main className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title}>Join Launchpad</h1>
        <p className={ui.hint}>
          Choose your name and a username. If you have an account already, leave both boxes empty.
        </p>
        <form onSubmit={(e) => void submit(e)} noValidate>
          <div className={ui.field}>
            <label htmlFor="invite-name">Name</label>
            <input
              id="invite-name"
              className={ui.input}
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="name"
              autoFocus
              aria-invalid={!!errors.name}
              aria-describedby={errors.name ? 'invite-name-error' : undefined}
            />
            {errors.name && (
              <span id="invite-name-error" className={styles.fieldError} aria-live="polite">
                {errors.name}
              </span>
            )}
          </div>
          <div className={ui.field}>
            <label htmlFor="invite-username">Username</label>
            <input
              id="invite-username"
              className={ui.input}
              value={username}
              onChange={(e) => setUsername(e.target.value.toLowerCase())}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              aria-invalid={!!errors.username}
              aria-describedby={errors.username ? 'invite-username-error' : undefined}
            />
            {errors.username && (
              <span id="invite-username-error" className={styles.fieldError} aria-live="polite">
                {errors.username}
              </span>
            )}
          </div>
          {banner && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {banner}
            </p>
          )}
          <button
            type="submit"
            className={`${ui.btn} ${ui.primary}`}
            disabled={busy}
            style={{ width: '100%', justifyContent: 'center', marginTop: 12 }}
          >
            {busy ? 'Joining…' : 'Join'}
          </button>
        </form>
      </div>
    </main>
  );
}

function Notice({
  title,
  headingRef,
  children,
}: {
  title: string;
  headingRef: RefObject<HTMLHeadingElement>;
  children: ReactNode;
}) {
  return (
    <main className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title} tabIndex={-1} ref={headingRef}>
          {title}
        </h1>
        {children}
      </div>
    </main>
  );
}
