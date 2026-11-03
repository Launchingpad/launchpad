import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { confirmSignup, requestSignup } from '../../api/auth';
import { ApiError } from '../../api/errors';
import { isModeRefusal, waitText } from '../settings/joinText';
import { useAppTitle } from '../../lib/documentTitle';
import styles from './setup.module.css';
import ui from '../../styles/ui.module.css';

// The public sign-up page (decisions-T12 #4 to #11). The rules are the server's (openapi
// SignupCreateRequest); the server stays the judge. The server answers 202 in every case, so the page shows
// the same "check your email" step whatever the address is, and it never says whether an email or a username
// exists. No text from the server reaches the screen.

const USERNAME_RE = /^[a-z][a-z0-9_-]{0,38}$/;
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
// The note allows a newline and a tab; no other control character.
// eslint-disable-next-line no-control-regex
const CONTROL_RE = /[\u0000-\u0008\u000b-\u001f\u007f]/;

type Field = 'email' | 'name' | 'username' | 'note';
type Values = Record<Field, string>;
type Errors = Partial<Record<Field, string>>;
const EMPTY: Values = { email: '', name: '', username: '', note: '' };
const ORDER: Field[] = ['email', 'name', 'username', 'note'];

export function validate(v: Values): Errors {
  const e: Errors = {};
  const email = v.email.trim();
  if (!EMAIL_RE.test(email) || email.length > 254) e.email = 'Enter a valid email address.';
  if (v.name.trim().length < 1 || [...v.name.trim()].length > 200) e.name = 'Enter 1 to 200 characters.';
  if (!USERNAME_RE.test(v.username.trim().toLowerCase()))
    e.username = 'Use 1 to 39 characters: a-z, 0-9, _ and -. Start with a letter.';
  if ([...v.note].length > 1000 || CONTROL_RE.test(v.note))
    e.note = 'Use 1000 characters or fewer, as plain text.';
  return e;
}

type Phase = 'form' | 'sent' | 'confirmed' | 'closed';

export function SignupPage() {
  useAppTitle('Sign up');
  const [phase, setPhase] = useState<Phase>('form');
  const [values, setValues] = useState<Values>(EMPTY);
  const [errors, setErrors] = useState<Errors>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [focusField, setFocusField] = useState<Field | null>(null);
  const [code, setCode] = useState('');
  const [codeError, setCodeError] = useState<string | null>(null);
  const [resent, setResent] = useState(false);
  const [wentBack, setWentBack] = useState(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const codeRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!focusField) return;
    document.getElementById(`signup-${focusField}`)?.focus();
    setFocusField(null);
  }, [focusField]);
  // A new step replaced the form: move focus to the code box (sent) or the heading, so a screen reader starts there.
  useEffect(() => {
    if (phase === 'sent') codeRef.current?.focus();
    else if (phase !== 'form') headingRef.current?.focus();
  }, [phase]);

  const set = (f: Field) => (value: string) =>
    setValues((v) => ({ ...v, [f]: f === 'username' ? value.toLowerCase() : value }));

  function bannerFor(err: unknown): string {
    if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 429) return `Too many requests. Try again in ${waitText(err.retryAfter)}.`;
    if (err.status === 400) return 'The server did not accept the request. Check each field and try again.';
    return 'Could not send the request. Try again.';
  }

  async function send(e?: FormEvent) {
    e?.preventDefault();
    const found = validate(values);
    setErrors(found);
    setBanner(null);
    if (Object.keys(found).length > 0) {
      setFocusField(ORDER.find((f) => found[f]) ?? null);
      return;
    }
    setBusy(true);
    try {
      await requestSignup({
        email: values.email.trim(),
        name: values.name.trim(),
        username: values.username.trim().toLowerCase(),
        ...(values.note.trim() ? { note: values.note.trim() } : {}),
      });
      setCode('');
      setCodeError(null);
      setPhase('sent');
    } catch (err) {
      if (isModeRefusal(err)) setPhase('closed');
      else setBanner(bannerFor(err));
    } finally {
      setBusy(false);
    }
  }

  async function resend() {
    setCodeError(null);
    setResent(false);
    setBusy(true);
    try {
      await requestSignup({
        email: values.email.trim(),
        name: values.name.trim(),
        username: values.username.trim().toLowerCase(),
        ...(values.note.trim() ? { note: values.note.trim() } : {}),
      });
      setResent(true);
    } catch (err) {
      if (isModeRefusal(err)) setPhase('closed');
      else setCodeError(bannerFor(err));
    } finally {
      setBusy(false);
    }
  }

  async function confirm(e: FormEvent) {
    e.preventDefault();
    setCodeError(null);
    setResent(false);
    if (!/^[0-9]{6}$/.test(code)) {
      setCodeError('Enter the 6 digits from the email.');
      codeRef.current?.focus();
      return;
    }
    setBusy(true);
    try {
      await confirmSignup(values.email.trim(), code);
      setCode('');
      setPhase('confirmed');
    } catch (err) {
      setCode('');
      if (isModeRefusal(err)) setPhase('closed');
      else if (err instanceof ApiError && err.status === 401)
        setCodeError('That code is not valid or has expired. Check it, or send a new code.');
      else if (err instanceof ApiError && err.status === 429)
        setCodeError(`Too many tries. Try again in ${waitText(err.retryAfter)}.`);
      else if (err instanceof ApiError && err.status === 400)
        setCodeError('Enter the 6 digits from the email.');
      else setCodeError(bannerFor(err));
      codeRef.current?.focus();
    } finally {
      setBusy(false);
    }
  }

  if (phase === 'closed') {
    return (
      <Notice title="Sign-up is not open" headingRef={headingRef}>
        <p>This install does not take sign-up requests now. Ask an admin for an invite.</p>
        <Link to="/login">Go to the login page</Link>
      </Notice>
    );
  }
  if (phase === 'confirmed') {
    return (
      <Notice title="Your email is confirmed" headingRef={headingRef}>
        <p>An admin must now approve your request. You have no access until then.</p>
        <p>After your request is approved, sign in with an email code.</p>
        <Link to="/login">Go to the login page</Link>
      </Notice>
    );
  }
  if (phase === 'sent') {
    return (
      <main className={styles.page}>
        <div className={styles.card}>
          <h1 className={styles.title} tabIndex={-1} ref={headingRef}>
            Check your email
          </h1>
          <p>
            Your request is sent. If the server can send email, it sent a 6-digit code to{' '}
            <strong>{values.email.trim()}</strong>. Enter the code to confirm your address.
          </p>
          <p className={ui.hint}>
            Look in your spam folder too. If the server cannot send email, you have no code. An admin can
            still see your request.
          </p>
          <form onSubmit={(e) => void confirm(e)} noValidate>
            <div className={ui.field}>
              <label htmlFor="signup-code">6-digit code</label>
              <input
                id="signup-code"
                ref={codeRef}
                className={ui.input}
                inputMode="numeric"
                autoComplete="one-time-code"
                maxLength={6}
                value={code}
                onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
                aria-invalid={!!codeError}
                aria-describedby={codeError ? 'signup-code-error' : undefined}
              />
            </div>
            {codeError && (
              <p id="signup-code-error" role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {codeError}
              </p>
            )}
            <button
              type="submit"
              className={`${ui.btn} ${ui.primary}`}
              disabled={busy}
              style={{ width: '100%', justifyContent: 'center', marginTop: 12 }}
            >
              {busy ? 'Checking…' : 'Confirm email'}
            </button>
          </form>
          <div className={styles.actions}>
            <button type="button" className={ui.btn} disabled={busy} onClick={() => void resend()}>
              Send a new code
            </button>
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost}`}
              onClick={() => {
                setPhase('form');
                setCodeError(null);
                setWentBack(true);
              }}
            >
              Use another email address
            </button>
          </div>
          <p role="status" className={ui.hint}>
            {resent ? 'If the server can send email, a new code is on its way.' : ''}
          </p>
        </div>
      </main>
    );
  }

  const text = (f: Field, label: string, extra: Record<string, unknown> = {}) => (
    <div className={ui.field}>
      <label htmlFor={`signup-${f}`}>{label}</label>
      <input
        id={`signup-${f}`}
        className={ui.input}
        value={values[f]}
        onChange={(e) => set(f)(e.target.value)}
        aria-invalid={!!errors[f]}
        aria-describedby={errors[f] ? `signup-${f}-error` : undefined}
        autoComplete="off"
        {...extra}
      />
      {errors[f] && (
        <span id={`signup-${f}-error`} className={styles.fieldError} aria-live="polite">
          {errors[f]}
        </span>
      )}
    </div>
  );

  return (
    <main className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title}>Ask to join Launchpad</h1>
        <p className={ui.hint}>
          Send a request. An admin decides. You get no access until an admin approves it. Have an invite link
          already? Open that link instead.
        </p>
        {wentBack && (
          <p className={ui.hint} data-testid="signup-went-back">
            A new name, username or note for the same email address is not used. The server keeps the first
            request. Only a different email address makes a new request.
          </p>
        )}
        <form onSubmit={(e) => void send(e)} noValidate>
          {text('email', 'Email', {
            type: 'email',
            inputMode: 'email',
            autoComplete: 'email',
            autoFocus: true,
          })}
          {text('name', 'Name', { autoComplete: 'name' })}
          {text('username', 'Username', { autoCapitalize: 'none', spellCheck: false })}
          <div className={ui.field}>
            <label htmlFor="signup-note">Note (optional)</label>
            <textarea
              id="signup-note"
              className={ui.textarea}
              value={values.note}
              onChange={(e) => set('note')(e.target.value)}
              aria-invalid={!!errors.note}
              aria-describedby={`signup-note-hint${errors.note ? ' signup-note-error' : ''}`}
              rows={3}
            />
            <span id="signup-note-hint" className={ui.hint}>
              Tell the admins who you are. Only admins read it.
            </span>
            {errors.note && (
              <span id="signup-note-error" className={styles.fieldError} aria-live="polite">
                {errors.note}
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
            {busy ? 'Sending…' : 'Send request'}
          </button>
        </form>
        <p className={ui.hint} style={{ marginTop: 12 }}>
          Already have an account? <Link to="/login">Sign in</Link>
        </p>
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
  headingRef: React.RefObject<HTMLHeadingElement>;
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
