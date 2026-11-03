import { useEffect, useId, useState, type FormEvent } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { ApiError, errorMessage } from '../../api/errors';
import { usePlatformSettings, useSavePlatformSettings, useTestPlatformMail } from '../../api/queries';
import { keys } from '../../api/keys';
import type { PlatformSettings, PlatformSettingsInput, Session } from '../../api/types';
import { Spinner } from '../../ui/States';
import { useIsPlatformAdmin } from './access';
import { PlatformAdminsSection } from './PlatformAdminsSection';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';
import { SettingsCard, SettingsPage, SettingsSection, NoAccess } from './SettingsParts';

// The Platform settings page. The rules are the server's (docs/internal/decisions-T9-mail-settings.md; openapi tag `platform`). The server
// stays the judge: the checks here only save a round trip. No text from the server reaches the screen (also not on a failed load),
// because a mail server may echo what it was sent. Every message below is fixed.

type Provider = PlatformSettings['mailProvider'];
type Security = PlatformSettings['smtpSecurity'];
type Signup = PlatformSettings['signupMode'];

const PROVIDERS: { value: Provider; label: string; text: string }[] = [
  {
    value: 'env',
    label: 'Server environment',
    text: 'Use the mail setup of the server (POSTMARK_SERVER_TOKEN). With no token, login is off.',
  },
  { value: 'none', label: 'None', text: 'Send no email. Login is off. Nobody can get a sign-in code.' },
  { value: 'postmark', label: 'Postmark', text: 'Send login email through Postmark.' },
  { value: 'smtp', label: 'SMTP', text: 'Send login email through an SMTP server.' },
];

const SECURITY: { value: Security; label: string }[] = [
  { value: 'starttls', label: 'STARTTLS (usually port 587)' },
  { value: 'tls', label: 'TLS from the first byte (usually port 465)' },
  { value: 'none', label: 'None (no TLS)' },
];

const SIGNUP: { value: Signup; label: string; text: string }[] = [
  { value: 'invite', label: 'Invite only', text: 'A new person needs an invite from an admin.' },
  {
    value: 'approval',
    label: 'Approval',
    text: 'A new person asks to join. An admin must approve the request.',
  },
  { value: 'closed', label: 'Closed', text: 'Nobody new can join.' },
];

const ACTIVE_TRANSPORT: Record<PlatformSettings['activeMail']['transport'], string> = {
  postmark: 'Postmark',
  smtp: 'SMTP',
  'dev-log': 'the server log (development only)',
  none: 'nothing (login email is off)',
};

// Field names in a 400 map to a fixed label. The server's own words are never shown.
const FIELD_LABELS: [string, string][] = [
  ['mailProvider', 'Mail provider'],
  ['postmarkFrom', 'Postmark sender'],
  ['postmarkMessageStream', 'Postmark message stream'],
  ['postmarkToken', 'Postmark token'],
  ['smtpHost', 'SMTP host'],
  ['smtpPort', 'SMTP port'],
  ['smtpSecurity', 'SMTP security'],
  ['smtpUsername', 'SMTP username'],
  ['smtpPassword', 'SMTP password'],
  ['smtpFromAddress', 'SMTP sender address'],
  ['smtpFromName', 'SMTP sender name'],
  ['signupMode', 'Sign-up mode'],
];

/** The labels of the known fields that a 400 message names. */
export function badFieldLabels(message: string): string[] {
  return FIELD_LABELS.filter(([name]) => message.includes(name)).map(([, label]) => label);
}

/** The fixed text for a failed save. */
export function saveErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return errorMessage(err);
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400: {
      const labels = badFieldLabels(err.message);
      return labels.length
        ? `The server did not accept the settings. Check: ${labels.join(', ')}.`
        : 'The server did not accept the settings. Check the fields.';
    }
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You cannot change the platform settings.';
    case 409:
      return 'This server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a secret. Ask the person who runs the install to set it.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return 'Could not save the settings. Try again.';
  }
}

/** The fixed text for a failed test email. */
export function testErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return errorMessage(err);
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return 'Your account has no email address, so the server cannot send a test email.';
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You cannot send a test email.';
    case 429:
      return 'You sent many test emails. Wait a minute and try again.';
    case 502:
      return 'The mail server did not accept the test email. Check the host, port, security and sign-in details.';
    case 503:
      return 'Login email is off. Choose Postmark or SMTP, save, and try again.';
    default:
      return 'The test email failed. Try again.';
  }
}

/** The fixed text for a failed load. */
export function loadErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return errorMessage(err);
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'The server says you are not a platform admin. If you became one after you signed in, sign out and sign in again.';
    default:
      return 'The server had a problem. Try again.';
  }
}

type SecretEdit = { value: string; clear: boolean };
const NO_EDIT: SecretEdit = { value: '', clear: false };

/** A write-only secret field: its state, a box for a new value, and a clear button. */
function SecretField({
  label,
  isSet,
  edit,
  onChange,
  disabled,
}: {
  label: string;
  isSet: boolean;
  edit: SecretEdit;
  onChange: (e: SecretEdit) => void;
  disabled: boolean;
}) {
  const id = useId();
  let status: string;
  if (edit.value !== '')
    status = isSet ? 'Set. It will be replaced when you save.' : 'Not set. It will be saved.';
  else if (edit.clear) status = 'Set. It will be cleared when you save.';
  else status = isSet ? 'Set' : 'Not set';
  return (
    <div className={ui.field}>
      <label htmlFor={id}>{label}</label>
      <div className={styles.secretRow}>
        <input
          id={id}
          type="password"
          className={ui.input}
          value={edit.value}
          onChange={(e) => onChange({ value: e.target.value, clear: false })}
          autoComplete="new-password"
          spellCheck={false}
          disabled={disabled}
          placeholder={isSet ? 'Type a new value to replace it' : ''}
          aria-describedby={`${id}-status`}
        />
        {isSet && !edit.clear && (
          <button
            type="button"
            className={ui.btn}
            disabled={disabled}
            onClick={() => onChange({ value: '', clear: true })}
          >
            Clear
          </button>
        )}
        {edit.clear && (
          <button type="button" className={ui.btn} onClick={() => onChange(NO_EDIT)}>
            Keep it
          </button>
        )}
      </div>
      <span id={`${id}-status`} className={ui.hint} data-testid={`status-${label}`}>
        {status}
      </span>
    </div>
  );
}

function TextField({
  label,
  value,
  onChange,
  hint,
  error,
  className,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  hint?: string;
  error?: string;
  className?: string;
}) {
  const id = useId();
  return (
    <div className={ui.field}>
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        className={`${ui.input} ${className ?? ''}`}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="off"
        spellCheck={false}
        aria-invalid={!!error}
        aria-describedby={error ? `${id}-e` : hint ? `${id}-h` : undefined}
      />
      {error && (
        <span id={`${id}-e`} className={styles.fieldError} aria-live="polite">
          {error}
        </span>
      )}
      {hint && !error && (
        <span id={`${id}-h`} className={ui.hint}>
          {hint}
        </span>
      )}
    </div>
  );
}

function LoginStatus({ s }: { s: PlatformSettings }) {
  const active = `${ACTIVE_TRANSPORT[s.activeMail.transport]}, from ${
    s.activeMail.source === 'settings' ? 'these settings' : 'the server environment'
  }`;
  return (
    <section aria-label="Login status" className={styles.statusBox}>
      {s.loginAvailable ? (
        <p role="status" data-testid="login-status">
          Login is on.
        </p>
      ) : (
        <div role="alert" className={`${ui.banner} ${ui.errorBanner}`} data-testid="login-warning">
          <div>
            <strong>Login is off. Nobody can get a sign-in code.</strong>{' '}
            {s.activeMail.transport === 'none'
              ? 'The server has no mail transport. Choose Postmark or SMTP below and save. Or choose "Server environment" and set POSTMARK_SERVER_TOKEN on the server.'
              : 'The server has mail, but login is still off. The server has no AUTH_OTP_PEPPER. Ask the person who runs the install to set it.'}
          </div>
        </div>
      )}
      <p className={ui.hint}>Login email now goes through {active}.</p>
      {s.secretsUnreadable && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          The server cannot open the stored secrets. The key may have changed. Enter the Postmark token and
          the SMTP password again, then save.
        </p>
      )}
      {!s.encryptionKeySet && (
        <p role="alert" className={ui.banner}>
          This server has no SETTINGS_ENCRYPTION_KEY. It cannot store a secret. Ask the person who runs the
          install to set it.
        </p>
      )}
    </section>
  );
}

function PlatformForm({
  settings,
  saved,
  onSaved,
}: {
  settings: PlatformSettings;
  saved: boolean;
  onSaved: () => void;
}) {
  const org = useOrgSlug();
  const save = useSavePlatformSettings();
  const sendTest = useTestPlatformMail();
  const [provider, setProvider] = useState<Provider>(settings.mailProvider);
  const [postmarkFrom, setPostmarkFrom] = useState(settings.postmarkFrom);
  const [postmarkStream, setPostmarkStream] = useState(settings.postmarkMessageStream);
  const [smtpHost, setSmtpHost] = useState(settings.smtpHost);
  const [portText, setPortText] = useState(String(settings.smtpPort));
  const [smtpSecurity, setSmtpSecurity] = useState<Security>(settings.smtpSecurity);
  const [smtpUsername, setSmtpUsername] = useState(settings.smtpUsername);
  const [smtpFromAddress, setSmtpFromAddress] = useState(settings.smtpFromAddress);
  const [smtpFromName, setSmtpFromName] = useState(settings.smtpFromName);
  const [signup, setSignup] = useState<Signup>(settings.signupMode);
  const [token, setToken] = useState<SecretEdit>(NO_EDIT);
  const [password, setPassword] = useState<SecretEdit>(NO_EDIT);
  const [portError, setPortError] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [testBusy, setTestBusy] = useState(false);
  const [test, setTest] = useState<{ ok: boolean; text: string } | null>(null);
  const providerId = useId();
  const secretsOff = !settings.encryptionKeySet;

  const dirty =
    provider !== settings.mailProvider ||
    postmarkFrom !== settings.postmarkFrom ||
    postmarkStream !== settings.postmarkMessageStream ||
    smtpHost !== settings.smtpHost ||
    portText !== String(settings.smtpPort) ||
    smtpSecurity !== settings.smtpSecurity ||
    smtpUsername !== settings.smtpUsername ||
    smtpFromAddress !== settings.smtpFromAddress ||
    smtpFromName !== settings.smtpFromName ||
    signup !== settings.signupMode ||
    token.value !== '' ||
    token.clear ||
    password.value !== '' ||
    password.clear;

  // A change after a test makes the old outcome stale.
  useEffect(() => {
    if (dirty) setTest(null);
  }, [dirty]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    // The port box is on screen for SMTP only, so only SMTP checks it. Another provider sends the stored port.
    const trimmed = portText.trim();
    const port = provider !== 'smtp' ? settings.smtpPort : trimmed === '' ? 0 : Number(trimmed);
    if (provider === 'smtp' && (!/^\d*$/.test(trimmed) || port > 65535)) {
      setPortError('Enter a whole number from 0 to 65535. Use 0 for the default, 587.');
      return;
    }
    setPortError(null);
    // The whole settings go every time (decisions-T9 #14). Absent keeps a stored secret, "" clears it,
    // a value replaces it (#11).
    const input: PlatformSettingsInput = {
      mailProvider: provider,
      postmarkFrom: postmarkFrom.trim(),
      postmarkMessageStream: postmarkStream.trim(),
      smtpHost: smtpHost.trim(),
      smtpPort: port,
      smtpSecurity,
      smtpUsername: smtpUsername.trim(),
      smtpFromAddress: smtpFromAddress.trim(),
      smtpFromName: smtpFromName.trim(),
      signupMode: signup,
    };
    if (token.value !== '') input.postmarkToken = token.value;
    else if (token.clear) input.postmarkToken = '';
    if (password.value !== '') input.smtpPassword = password.value;
    else if (password.clear) input.smtpPassword = '';
    setBusy(true);
    try {
      await save(input);
      // The secrets leave the page at once. The parent remounts this form with the saved settings.
      setToken(NO_EDIT);
      setPassword(NO_EDIT);
      onSaved();
    } catch (err) {
      setBanner(saveErrorText(err));
    } finally {
      setBusy(false);
    }
  }

  async function runTest() {
    setTest(null);
    setTestBusy(true);
    try {
      const r = await sendTest();
      setTest({ ok: true, text: `Test email sent to ${r.to}. Check your inbox.` });
    } catch (err) {
      setTest({ ok: false, text: testErrorText(err) });
    } finally {
      setTestBusy(false);
    }
  }

  const current = PROVIDERS.find((p) => p.value === provider);
  return (
    <SettingsSection>
      <SettingsCard padded>
        <form onSubmit={(e) => void submit(e)} noValidate aria-label="Platform settings">
          <LoginStatus s={settings} />

          <h2 className={styles.subtitle}>Login email</h2>
          <div className={ui.field}>
            <label htmlFor={providerId}>Mail provider</label>
            <select
              id={providerId}
              className={ui.select}
              value={provider}
              onChange={(e) => {
                setProvider(e.target.value as Provider);
                // A bad port typed for SMTP is not kept when the box goes away.
                setPortText(String(settings.smtpPort));
                setPortError(null);
                // A secret typed for one provider is not kept for another.
                setToken(NO_EDIT);
                setPassword(NO_EDIT);
              }}
            >
              {PROVIDERS.map((p) => (
                <option key={p.value} value={p.value}>
                  {p.label}
                </option>
              ))}
            </select>
            <span className={ui.hint}>{current?.text}</span>
          </div>

          {provider === 'postmark' && (
            <>
              <SecretField
                label="Postmark server token"
                isSet={settings.postmarkTokenSet}
                edit={token}
                onChange={setToken}
                disabled={secretsOff}
              />
              <TextField
                label="Sender (From)"
                value={postmarkFrom}
                onChange={setPostmarkFrom}
                hint="An address, or Name <address>. Postmark must know this sender."
              />
              <TextField
                label="Message stream"
                value={postmarkStream}
                onChange={setPostmarkStream}
                hint="Leave empty for the outbound stream."
              />
            </>
          )}

          {provider === 'smtp' && (
            <>
              <TextField
                label="SMTP host"
                value={smtpHost}
                onChange={setSmtpHost}
                hint="A host name or IP address. No scheme and no port."
              />
              <TextField
                label="SMTP port"
                value={portText}
                onChange={setPortText}
                className={styles.num}
                error={portError ?? undefined}
                hint="Use 0 for the default, 587."
              />
              <div className={ui.field}>
                <label htmlFor={`${providerId}-sec`}>SMTP security</label>
                <select
                  id={`${providerId}-sec`}
                  className={ui.select}
                  value={smtpSecurity}
                  onChange={(e) => setSmtpSecurity(e.target.value as Security)}
                >
                  {SECURITY.map((s) => (
                    <option key={s.value} value={s.value}>
                      {s.label}
                    </option>
                  ))}
                </select>
                <span className={ui.hint}>The server sends a password only over TLS, or to localhost.</span>
              </div>
              <TextField
                label="SMTP username"
                value={smtpUsername}
                onChange={setSmtpUsername}
                hint="Leave empty to send with no sign-in."
              />
              <SecretField
                label="SMTP password"
                isSet={settings.smtpPasswordSet}
                edit={password}
                onChange={setPassword}
                disabled={secretsOff}
              />
              <TextField
                label="Sender address"
                value={smtpFromAddress}
                onChange={setSmtpFromAddress}
                hint="A plain address, for example login@example.com."
              />
              <TextField label="Sender name" value={smtpFromName} onChange={setSmtpFromName} />
            </>
          )}

          <fieldset className={styles.radios}>
            <legend>Sign-up mode</legend>
            {SIGNUP.map((s) => (
              <label key={s.value} className={styles.radioFull}>
                <input
                  type="radio"
                  name="signup-mode"
                  checked={signup === s.value}
                  onChange={() => setSignup(s.value)}
                />
                <span>
                  <strong>{s.label}.</strong> {s.text}
                </span>
              </label>
            ))}
            <span className={ui.hint}>
              The server uses a saved mode at once. Invite only: an admin invites each person on the{' '}
              <Link to={`/${org}/settings/members`}>Members page</Link>. Approval: invites work, and a person
              can also ask to join on the <Link to="/signup">sign-up page</Link>. An org admin decides on the{' '}
              <Link to={`/${org}/settings/signups`}>Sign-ups page</Link>. Closed: invites and sign-up requests
              are both off.
            </span>
          </fieldset>

          {banner && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {banner}
            </p>
          )}
          <div className={styles.actions}>
            <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy || !dirty}>
              {busy ? 'Saving…' : 'Save settings'}
            </button>
            {saved && !dirty && (
              <span role="status" className={ui.hint}>
                Settings saved.
              </span>
            )}
          </div>

          <h2 className={`${styles.subtitle} ${styles.section}`}>Test</h2>
          <p className={ui.hint}>
            The test sends one email to your own address. It uses the saved settings. Save first.
          </p>
          <div className={styles.actions}>
            <button
              type="button"
              className={ui.btn}
              disabled={testBusy || dirty}
              onClick={() => void runTest()}
            >
              {testBusy ? 'Sending…' : 'Send test email'}
            </button>
          </div>
          <div role="status" aria-live="polite" data-testid="test-result">
            {test && <p className={test.ok ? undefined : `${ui.banner} ${ui.errorBanner}`}>{test.text}</p>}
          </div>
        </form>
      </SettingsCard>
    </SettingsSection>
  );
}

export function PlatformPage() {
  const qc = useQueryClient();
  const isPlatformAdmin = useIsPlatformAdmin();
  // A platform admin who removes themselves: the session still says "admin" until the next sign-in, so say what
  // happened here and drop the flag from the cached session. Without this the next load shows a 403 text.
  const [left, setLeft] = useState(false);
  function selfRemoved() {
    setLeft(true);
    qc.setQueryData<Session | null>(keys.session, (s) =>
      s ? { ...s, user: { ...s.user, isPlatformAdmin: false } } : s,
    );
  }
  if (left) {
    return (
      <NoAccess
        title="You are no longer a platform admin."
        hint="You cannot open the platform pages now. Another platform admin can add you again."
      />
    );
  }
  // The nav item is hidden from other people, but a direct visit must still be refused.
  if (!isPlatformAdmin) {
    return (
      <NoAccess
        title="You need platform admin access."
        hint="Only the person who runs this install can open this page."
      />
    );
  }
  return <PlatformSettingsView onSelfRemoved={selfRemoved} />;
}

function PlatformSettingsView({ onSelfRemoved }: { onSelfRemoved: () => void }) {
  const q = usePlatformSettings();
  const [saved, setSaved] = useState(false);
  return (
    <SettingsPage
      title="Platform"
      description={<>These settings cover the whole install, not one organisation.</>}
      wide
    >
      {q.isPending && <Spinner label="Loading the platform settings" />}
      {q.isError && (
        <div role="alert" className={`${ui.banner} ${ui.errorBanner}`} data-testid="load-error">
          <div>
            <strong>Could not load the settings.</strong> {loadErrorText(q.error)}
          </div>
          <button type="button" className={ui.btn} onClick={() => void q.refetch()}>
            Try again
          </button>
        </div>
      )}
      {q.data && (
        <PlatformForm key={q.data.updatedAt} settings={q.data} saved={saved} onSaved={() => setSaved(true)} />
      )}
      <PlatformAdminsSection onSelfRemoved={onSelfRemoved} />
    </SettingsPage>
  );
}
