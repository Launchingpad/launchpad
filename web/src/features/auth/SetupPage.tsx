import { useEffect, useRef, useState, type FormEvent, type ReactNode, type RefObject } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { completeSetup, fetchSetupRequired, type SetupResult } from '../../api/auth';
import { ApiError } from '../../api/errors';
import { keys } from '../../api/keys';
import { Spinner } from '../../ui/States';
import { useAppTitle } from '../../lib/documentTitle';
import styles from './setup.module.css';
import ui from '../../styles/ui.module.css';

// The rules are the server's (decisions-T8 #2, openapi SetupRequest). The server stays the judge.
const USERNAME_RE = /^[a-z][a-z0-9_-]{0,38}$/;
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$/;
const KEY_RE = /^[A-Z][A-Z0-9]{0,9}$/;
const TOKEN_RE = /^[A-Za-z0-9_-]{43}$/;

type Field = 'username' | 'name' | 'email' | 'orgName' | 'slug' | 'teamName' | 'key';
type Values = Record<Field, string>;
type Errors = Partial<Record<Field, string>>;

const FIELD_ORDER: Field[] = ['username', 'name', 'email', 'orgName', 'slug', 'teamName', 'key'];
const EMPTY: Values = { username: '', name: '', email: '', orgName: '', slug: '', teamName: '', key: '' };

// The link token lives in this variable and in component state only. Never in storage, a URL or a message.
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
export function forgetSetupToken(): void {
  heldToken = undefined;
}

export function validate(v: Values): Errors {
  const e: Errors = {};
  if (!USERNAME_RE.test(v.username.trim().toLowerCase()))
    e.username = 'Use 1 to 39 characters: a-z, 0-9, _ and -. Start with a letter.';
  if (v.name.trim().length < 1 || [...v.name.trim()].length > 200) e.name = 'Enter 1 to 200 characters.';
  const email = v.email.trim();
  if (!EMAIL_RE.test(email) || email.length > 254) e.email = 'Enter a valid email address.';
  if (v.orgName.trim().length < 1 || [...v.orgName.trim()].length > 200)
    e.orgName = 'Enter 1 to 200 characters.';
  if (!SLUG_RE.test(v.slug.trim()))
    e.slug = 'Use 2 to 40 characters: a-z, 0-9 and -. Start and end with a letter or digit.';
  if (v.teamName.trim().length < 1 || [...v.teamName.trim()].length > 200)
    e.teamName = 'Enter 1 to 200 characters.';
  if (!KEY_RE.test(v.key.trim())) e.key = 'Use 1 to 10 characters: A-Z and 0-9. Start with a letter.';
  return e;
}

/** The 400 text names the field it rejects (`user.email must be ...`, `org slug must be ...`). */
const PREFIXES: [string, Field][] = [
  ['user.username', 'username'],
  ['user.name', 'name'],
  ['user.email', 'email'],
  ['org slug', 'slug'],
  ['org name', 'orgName'],
  ['team.key', 'key'],
  ['team.name', 'teamName'],
];

export function fieldFromMessage(message: string): Field | undefined {
  return PREFIXES.find(([p]) => message.startsWith(p))?.[1];
}

function waitText(seconds?: number): string {
  if (!seconds) return 'a little while';
  if (seconds === 1) return '1 second';
  if (seconds < 90) return `${seconds} seconds`;
  return `${Math.ceil(seconds / 60)} minutes`;
}

type Phase = 'form' | 'invalid' | 'complete' | 'token';

export function SetupPage() {
  useAppTitle('Set up');
  const qc = useQueryClient();
  const nav = useNavigate();
  const [token, setToken] = useState(takeToken);
  const [focusField, setFocusField] = useState<Field | null>(null);
  const [phase, setPhase] = useState<Phase>('form');
  const [values, setValues] = useState<Values>(EMPTY);
  const [errors, setErrors] = useState<Errors>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<SetupResult | null>(null);
  const [saved, setSaved] = useState(false);
  const [copyNote, setCopyNote] = useState<string | null>(null);

  const status = useQuery({
    queryKey: ['setup-required'],
    queryFn: fetchSetupRequired,
    retry: false,
    staleTime: 0,
    refetchOnWindowFocus: false,
  });

  // A new link opened in this tab changes only the fragment: the page stays mounted. Take the new token,
  // remove it from the URL again and start over from the form.
  useEffect(() => {
    const onHash = () => {
      if (!window.location.hash.replace(/^#/, '')) return;
      setToken(takeToken());
      setPhase((p) => (p === 'token' ? p : 'form'));
      setErrors({});
      setBanner(null);
      void qc.invalidateQueries({ queryKey: ['setup-required'] });
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, [qc]);

  // Move focus after the errors are on screen (state updates are batched, so the DOM is not ready in the handler).
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(`setup-${focusField}`)?.focus();
    setFocusField(null);
  }, [focusField]);

  // Forget the link token when the page goes away.
  useEffect(() => forgetSetupToken, []);
  useEffect(() => {
    if (status.data === false) heldToken = undefined;
  }, [status.data]);

  // A new page replaced the form: move focus to its heading so a screen reader starts there.
  const headingRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (phase !== 'form') headingRef.current?.focus();
  }, [phase]);

  // Once the secret is on screen, warn before the page closes: the server never shows it again.
  useEffect(() => {
    if (phase !== 'token') return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [phase]);

  const set = (f: Field) => (value: string) =>
    setValues((v) => ({
      ...v,
      [f]: f === 'key' ? value.toUpperCase() : f === 'slug' ? value.toLowerCase() : value,
    }));

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!token) return;
    const found = validate(values);
    setErrors(found);
    setBanner(null);
    if (Object.keys(found).length > 0) {
      setFocusField(FIELD_ORDER.find((f) => found[f]) ?? null);
      return;
    }
    setBusy(true);
    try {
      const r = await completeSetup({
        token,
        user: {
          username: values.username.trim().toLowerCase(),
          name: values.name.trim(),
          email: values.email.trim(),
        },
        org: { slug: values.slug.trim().toLowerCase(), name: values.orgName.trim() },
        team: { key: values.key.trim(), name: values.teamName.trim() },
      });
      heldToken = undefined;
      setResult(r);
      setPhase('token');
    } catch (err) {
      handleError(err);
    } finally {
      setBusy(false);
    }
  }

  function fieldError(field: Field, text: string) {
    setErrors({ [field]: text });
    setFocusField(field);
  }

  // Server text is shown only for a 400, with the link token cut out. Every other failure gets a fixed text,
  // so nothing the server sends can put the token on screen.
  function handleError(err: unknown) {
    if (result) return; // the first token is on screen: never replace the only copy
    if (!(err instanceof ApiError)) {
      setBanner('Something went wrong. Try again.');
      return;
    }
    if (err.status === 401 && err.code === 'invalid_setup_token') {
      heldToken = undefined;
      setPhase('invalid');
    } else if (err.status === 409 && err.code === 'setup_complete') {
      heldToken = undefined;
      setPhase('complete');
    } else if (err.status === 409) {
      fieldError('slug', 'An organisation with this slug exists already. Use another slug.');
    } else if (err.status === 429) {
      setBanner(`Too many attempts. Try again in ${waitText(err.retryAfter)}.`);
    } else if (err.status === 400) {
      const text = token ? err.message.split(token).join('') : err.message;
      const field = fieldFromMessage(text);
      if (field) fieldError(field, validate(values)[field] ?? text);
      else setBanner(text);
    } else if (err.status === 0) {
      setBanner(err.message); // fixed client text: could not reach the server
    } else {
      setBanner('Setup failed. Try again. If it fails again, read the server log.');
    }
  }

  async function copy() {
    if (!result) return;
    try {
      await navigator.clipboard.writeText(result.tokenSecret);
      setCopyNote('Copied.');
    } catch {
      setCopyNote('Could not copy. Select the token and copy it by hand.');
    }
  }

  function proceed() {
    if (!result) return;
    const { tokenSecret: _drop, ...session } = result;
    void _drop;
    qc.setQueryData(keys.session, session);
    setResult(null);
    nav(`/${session.orgs[0]?.slug ?? ''}/settings/setup`, { replace: true });
  }

  if (phase === 'token' && result) {
    return (
      <main className={styles.page}>
        <div className={styles.card}>
          <h1 className={styles.title} tabIndex={-1} ref={headingRef}>
            Setup is done
          </h1>
          <p>
            Welcome, {result.user.name}. This is your first admin token. It has read, write and admin access
            to {result.orgs[0]?.name ?? 'your organisation'}.
          </p>
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            Save this token now. It is not shown again.
          </p>
          <code className={styles.tokenBox} data-testid="first-token">
            {result.tokenSecret}
          </code>
          <div className={styles.actions}>
            <button type="button" className={ui.btn} onClick={() => void copy()}>
              Copy token
            </button>
            {copyNote && (
              <span role="status" className={ui.hint}>
                {copyNote}
              </span>
            )}
          </div>
          <label className={styles.confirm}>
            <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.target.checked)} />
            <span>I saved the token in a safe place.</span>
          </label>
          <button
            type="button"
            className={`${ui.btn} ${ui.primary}`}
            disabled={!saved}
            onClick={proceed}
            style={{ width: '100%', justifyContent: 'center' }}
          >
            Continue to Launchpad
          </button>
        </div>
      </main>
    );
  }

  if (phase === 'complete' || (phase === 'form' && status.data === false)) {
    return (
      <Notice headingRef={headingRef} title="Setup is already done">
        <p>This install has a user. Sign in to continue.</p>
        <Link to="/login">Go to the login page</Link>
      </Notice>
    );
  }
  if (phase === 'invalid') {
    return (
      <Notice headingRef={headingRef} title="This setup link does not work">
        <p>The link is wrong, expired, used or replaced. Each link works one time, for 24 hours.</p>
        <NewLinkHelp />
      </Notice>
    );
  }
  if (!token) {
    return (
      <Notice headingRef={headingRef} title="You need a setup link">
        <p>Open the link from the server log. Do not type it by hand.</p>
        <NewLinkHelp />
      </Notice>
    );
  }
  if (!TOKEN_RE.test(token)) {
    return (
      <Notice headingRef={headingRef} title="This setup link is not complete">
        <p>Copy the whole link, to the last character.</p>
        <NewLinkHelp />
      </Notice>
    );
  }
  if (status.isPending) return <Spinner label="Checking setup" />;

  const text = (f: Field, label: string, extra: Record<string, unknown> = {}) => (
    <div className={ui.field}>
      <label htmlFor={`setup-${f}`}>{label}</label>
      <input
        id={`setup-${f}`}
        className={ui.input}
        value={values[f]}
        onChange={(e) => set(f)(e.target.value)}
        aria-invalid={!!errors[f]}
        aria-describedby={
          [errors[f] ? `setup-${f}-error` : '', f === 'key' ? 'setup-key-hint' : '']
            .filter(Boolean)
            .join(' ') || undefined
        }
        autoComplete="off"
        {...extra}
      />
      {errors[f] && (
        <span id={`setup-${f}-error`} className={styles.fieldError} aria-live="polite">
          {errors[f]}
        </span>
      )}
    </div>
  );

  return (
    <main className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title}>
          Set up Launchpad <span aria-hidden="true">🚀</span>
        </h1>
        <p className={ui.hint}>
          This makes the first user, the organisation and the first team. You do this one time.
        </p>
        <form onSubmit={(e) => void submit(e)} noValidate>
          <fieldset className={styles.group}>
            <legend>Your account</legend>
            {text('username', 'Username', { autoFocus: true, autoCapitalize: 'none', spellCheck: false })}
            {text('name', 'Name', { autoComplete: 'name' })}
            {text('email', 'Email', { type: 'email', inputMode: 'email', autoComplete: 'email' })}
          </fieldset>
          <fieldset className={styles.group}>
            <legend>Your organisation</legend>
            <div className={styles.pair}>
              {text('orgName', 'Organisation name')}
              {text('slug', 'Slug', { autoCapitalize: 'none', spellCheck: false })}
            </div>
          </fieldset>
          <fieldset className={styles.group}>
            <legend>Your first team</legend>
            <div className={styles.pair}>
              {text('teamName', 'Team name')}
              {text('key', 'Team key', { autoCapitalize: 'characters', spellCheck: false })}
            </div>
            <p id="setup-key-hint" className={ui.hint}>
              The key starts every issue number, for example ORB-12.
            </p>
          </fieldset>
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
            {busy ? 'Setting up…' : 'Set up Launchpad'}
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

function NewLinkHelp() {
  return (
    <>
      <p>To get a link, read the server log. Look for the message “setup: open”.</p>
      <p>
        Or run <code>./deploy/deploy.sh --setup-link</code> on the server.
      </p>
    </>
  );
}
