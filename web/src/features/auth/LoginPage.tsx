import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { requestOtp, verifyOtp } from '../../api/auth';
import { ApiError, errorMessage } from '../../api/errors';
import { keys } from '../../api/keys';
import { usingBearer } from '../../api/http';
import { useSession } from './session';
import { useAppTitle } from '../../lib/documentTitle';
import styles from './login.module.css';
import ui from '../../styles/ui.module.css';

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

function waitText(seconds?: number): string {
  if (!seconds) return 'a little while';
  if (seconds < 90) return `${seconds} seconds`;
  return `${Math.ceil(seconds / 60)} minutes`;
}

/** What to tell the person. Nothing here depends on whether the address has an account. */
export function loginErrorText(err: unknown, step: 'email' | 'code'): string {
  if (err instanceof ApiError) {
    if (err.status === 429) return `Too many attempts. Try again in ${waitText(err.retryAfter)}.`;
    if (err.status === 0 || err.status >= 500) return err.message;
    return step === 'code'
      ? 'That code is not valid or has expired. Check it, or ask for a new one.'
      : 'We could not send a code. Check the address and try again.';
  }
  return errorMessage(err);
}

export function LoginPage() {
  useAppTitle('Log in');
  const session = useSession();
  const qc = useQueryClient();
  const nav = useNavigate();
  const loc = useLocation();
  const from = (loc.state as { from?: string } | null)?.from ?? '/';
  const [step, setStep] = useState<'email' | 'code'>('email');
  const [email, setEmail] = useState('');
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [cooldown, setCooldown] = useState(0);
  const codeRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (cooldown <= 0) return;
    const t = setTimeout(() => setCooldown((c) => c - 1), 1000);
    return () => clearTimeout(t);
  }, [cooldown]);
  useEffect(() => {
    if (step === 'code') codeRef.current?.focus();
  }, [step]);

  if (session.data || usingBearer()) return <Navigate to={from} replace />;

  async function send(e?: FormEvent) {
    e?.preventDefault();
    const addr = email.trim();
    if (!EMAIL_RE.test(addr)) {
      setError('Enter a valid email address.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await requestOtp(addr);
      setStep('code');
      setCode('');
      setCooldown(30);
      setNotice('If that address can sign in, a 6-digit code is on its way.');
    } catch (err) {
      setError(loginErrorText(err, 'email'));
    } finally {
      setBusy(false);
    }
  }

  async function verify(value: string) {
    setBusy(true);
    setError(null);
    try {
      const r = await verifyOtp(email.trim(), value);
      qc.setQueryData(keys.session, { user: r.user, orgs: r.orgs });
      nav(from, { replace: true });
    } catch (err) {
      setError(loginErrorText(err, 'code'));
      setCode('');
      codeRef.current?.focus();
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title}>Sign in to Launchpad 🚀</h1>
        {step === 'email' ? (
          <form onSubmit={send} noValidate>
            <p className={ui.hint}>We email you a one-time code. No password.</p>
            <div className={ui.field}>
              <label htmlFor="email">Email</label>
              <input
                id="email"
                className={ui.input}
                type="email"
                autoComplete="email"
                inputMode="email"
                autoFocus
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                aria-invalid={!!error}
                aria-describedby={error ? 'login-error' : undefined}
              />
            </div>
            {error && (
              <p id="login-error" role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {error}
              </p>
            )}
            <button
              type="submit"
              className={`${ui.btn} ${ui.primary}`}
              disabled={busy}
              style={{ width: '100%', justifyContent: 'center', marginTop: 12 }}
            >
              {busy ? 'Sending…' : 'Email me a code'}
            </button>
            {/* The web app cannot read the sign-up mode while signed out (no public route), so the link is
                always here. The sign-up page shows a plain notice when the mode is not Approval. */}
            <p className={ui.hint} style={{ marginTop: 12 }}>
              No account yet? <Link to="/signup">Ask to join</Link>
            </p>
          </form>
        ) : (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (code.length === 6) void verify(code);
            }}
            noValidate
          >
            {notice && <p className={ui.hint}>{notice}</p>}
            <div className={ui.field}>
              <label htmlFor="code">6-digit code</label>
              <input
                id="code"
                ref={codeRef}
                className={`${ui.input} ${styles.code}`}
                inputMode="numeric"
                autoComplete="one-time-code"
                pattern="[0-9]*"
                maxLength={6}
                value={code}
                onChange={(e) => {
                  const v = e.target.value.replace(/\D/g, '').slice(0, 6);
                  setCode(v);
                  if (v.length === 6 && !busy) void verify(v);
                }}
                aria-invalid={!!error}
                aria-describedby={error ? 'login-error' : undefined}
              />
            </div>
            {error && (
              <p id="login-error" role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {error}
              </p>
            )}
            <button
              type="submit"
              className={`${ui.btn} ${ui.primary}`}
              disabled={busy || code.length !== 6}
              style={{ width: '100%', justifyContent: 'center', marginTop: 12 }}
            >
              {busy ? 'Checking…' : 'Sign in'}
            </button>
            <div className={styles.row}>
              <button
                type="button"
                className={`${ui.btn} ${ui.ghost}`}
                disabled={busy || cooldown > 0}
                onClick={() => void send()}
              >
                {cooldown > 0 ? `Send a new code (${cooldown}s)` : 'Send a new code'}
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.ghost}`}
                onClick={() => {
                  setStep('email');
                  setError(null);
                }}
              >
                Use another email
              </button>
            </div>
          </form>
        )}
      </div>
    </main>
  );
}
