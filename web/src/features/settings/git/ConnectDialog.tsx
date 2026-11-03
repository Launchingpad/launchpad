import { useEffect, useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { ApiError } from '../../../api/errors';
import { useCreateGitIntegration, useGitIntegrations } from '../../../api/gitIntegrations';
import { useOrgSlug } from '../../../api/org';
import type { GitIntegrationCreated } from '../../../api/types';
import { Dialog } from '../../../ui/Dialog';
import {
  FIELD_TEXT,
  PROVIDERS,
  badField,
  checkBaseUrl,
  gitErrorText,
  providerInfo,
  type ProviderId,
} from './gitText';
import { SecretInput, SecretOnceDialog, WebhookUrl, touchClass } from './gitParts';
import styles from './git.module.css';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

type Step = 1 | 2 | 3;
type Mode = 'token' | 'webhook_only';
type Field = 'baseUrl' | 'token' | 'secret' | 'name';

/** The connect dialog, three steps (decisions-T17 #46): provider and address, mode, done. */
export function ConnectDialog({ onClose }: { onClose: () => void }) {
  const org = useOrgSlug();
  const create = useCreateGitIntegration();
  const existing = useGitIntegrations();
  const [step, setStep] = useState<Step>(1);
  const [provider, setProvider] = useState<ProviderId>(PROVIDERS[0]!.id);
  const info = providerInfo(provider)!;
  const [selfHosted, setSelfHosted] = useState(false);
  const [baseUrl, setBaseUrl] = useState('');
  const [name, setName] = useState('');
  const [mode, setMode] = useState<Mode>('token');
  // Write-only fields. They live here, and are cleared as soon as the call has been sent.
  const [token, setToken] = useState('');
  const [pasted, setPasted] = useState('');
  const [errors, setErrors] = useState<Partial<Record<Field, string>>>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // LAUN-104: the 409 integration_exists case points at the connection that is already there.
  const [duplicate, setDuplicate] = useState(false);
  // The 201 body, secret included. Component state only; the dialog's unmount drops it.
  const [created, setCreated] = useState<GitIntegrationCreated | null>(null);
  const [focusId, setFocusId] = useState<string | null>(null);

  // A close while the create is in flight would unmount the dialog and lose the one-time secret of the 201.
  const close = () => {
    if (!busy) onClose();
  };

  const address = selfHosted ? baseUrl : info.hostedUrl;
  const safeAddress = checkBaseUrl(address) === null && address.trim().startsWith('https://');

  useEffect(() => {
    if (!focusId) return;
    document.getElementById(focusId)?.focus();
    setFocusId(null);
  }, [focusId]);

  function fail(field: Field, text: string, id: string) {
    setErrors({ [field]: text });
    setFocusId(id);
  }

  function next(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    const bad = checkBaseUrl(address);
    if (bad) return fail('baseUrl', bad, 'git-base-url');
    setErrors({});
    setStep(2);
    setFocusId('git-mode-token');
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    setBanner(null);
    setDuplicate(false);
    const t = token;
    if (mode === 'token' && t.trim() === '') return fail('token', 'Enter the token.', 'git-token');
    setErrors({});
    setBusy(true);
    try {
      const body = {
        provider,
        baseUrl: address.trim(),
        mode,
        enabled: true,
        ...(name.trim() ? { name: name.trim() } : {}),
        ...(mode === 'token' ? { token: t } : {}),
        ...(mode === 'webhook_only' && provider === 'gitlab' && pasted.trim()
          ? { secret: pasted.trim() }
          : {}),
      };
      setCreated(await create(body));
      setStep(3);
    } catch (err) {
      handleError(err);
    } finally {
      // Whatever happened, the typed secrets are gone from this component.
      setToken('');
      setPasted('');
      setBusy(false);
    }
  }

  // The server's words are never shown. A 400 maps to a field and a fixed sentence; the rest to a fixed banner.
  function handleError(err: unknown) {
    const f = badField(err);
    if (f === 'baseUrl' || f === 'apiBaseUrl') {
      setStep(1);
      return fail('baseUrl', FIELD_TEXT.baseUrl, 'git-base-url');
    }
    if (f === 'secret') return fail('secret', FIELD_TEXT.secret, 'git-pasted-secret');
    if (f === 'token') return fail('token', FIELD_TEXT.token, 'git-token');
    if (f === 'name') {
      setStep(1);
      return fail('name', FIELD_TEXT.name, 'git-name');
    }
    setBanner(gitErrorText(err, 'connect', provider));
    setDuplicate(err instanceof ApiError && err.code === 'integration_exists');
    if (err instanceof ApiError && err.code === 'provider_unauthorized') setFocusId('git-token');
  }

  const hostOf = (u: string) =>
    u
      .trim()
      .replace(/^https?:\/\//i, '')
      .replace(/\/+$/, '')
      .replace(/:443$/, '')
      .toLowerCase();
  const match = existing.data?.find((c) => c.provider === provider && hostOf(c.baseUrl) === hostOf(address));
  const existingPath = match ? `/${org}/settings/integrations/${match.id}` : `/${org}/settings/integrations`;

  if (step === 3 && created) {
    const webhookOnly = created.mode === 'webhook_only';
    const hasSecret = !!created.webhookSecret;
    const done = (
      <p className={ui.hint}>
        <Link to={`/${org}/settings/integrations/${created.id}`} onClick={onClose}>
          Open the connection
        </Link>{' '}
        to map repositories.
      </p>
    );
    if (hasSecret)
      return (
        <SecretOnceDialog
          title="Copy the webhook secret"
          secret={created.webhookSecret!}
          confirmLabel="I saved the secret in a safe place."
          onDone={onClose}
        >
          <h3 className={settings.subtitle}>Set up the hook on {info.label}</h3>
          <p className={ui.hint}>Add a webhook to each repository. Use this address:</p>
          <WebhookUrl url={created.webhookUrl} />
          <ul className={styles.hookList}>
            {info.hookHelp.map((h) => (
              <li key={h}>{h}</li>
            ))}
          </ul>
          {done}
        </SecretOnceDialog>
      );
    return (
      <Dialog
        className={touchClass}
        title="Connected"
        width={560}
        onClose={onClose}
        footer={
          <button type="button" className={`${ui.btn} ${ui.primary}`} data-autofocus onClick={onClose}>
            Done
          </button>
        }
      >
        <p>
          {info.label} is connected{webhookOnly ? ' in webhook-only mode' : ''}.{' '}
          {webhookOnly
            ? 'You used your own secret, so Launchpad shows no new one. Add the hook on the host with this address:'
            : 'Launchpad checked the token.'}
        </p>
        {webhookOnly && (
          <>
            <WebhookUrl url={created.webhookUrl} />
            <ul className={styles.hookList}>
              {info.hookHelp.map((h) => (
                <li key={h}>{h}</li>
              ))}
            </ul>
          </>
        )}
        {done}
      </Dialog>
    );
  }

  const stepNames = ['Provider', 'Mode', 'Done'];
  return (
    <Dialog className={touchClass} title="Connect a git host" width={560} onClose={close}>
      <ol className={styles.steps} aria-label="Steps">
        {stepNames.map((n, i) => (
          <li
            key={n}
            className={step === i + 1 ? styles.stepOn : undefined}
            aria-current={step === i + 1 ? 'step' : undefined}
          >
            {i + 1}. {n}
          </li>
        ))}
      </ol>
      {step === 1 && (
        <form onSubmit={next} noValidate aria-label="Provider and address">
          <fieldset className={settings.radios}>
            <legend>Provider</legend>
            {PROVIDERS.map((p) => (
              <label key={p.id}>
                <input
                  id={`git-provider-${p.id}`}
                  type="radio"
                  name="git-provider"
                  checked={provider === p.id}
                  data-autofocus={provider === p.id ? '' : undefined}
                  onChange={() => {
                    setProvider(p.id);
                    // The own-secret field exists for GitLab only: a secret pasted there must not follow the switch.
                    setPasted('');
                    setErrors({});
                  }}
                />
                <span>{p.label}</span>
              </label>
            ))}
          </fieldset>
          <label className={settings.confirm}>
            <input
              type="checkbox"
              checked={selfHosted}
              onChange={(e) => {
                setSelfHosted(e.target.checked);
                setErrors({});
              }}
            />
            <span>{info.selfHostedLabel}</span>
          </label>
          {selfHosted ? (
            <div className={ui.field}>
              <label htmlFor="git-base-url">Web address of the host</label>
              <input
                id="git-base-url"
                className={ui.input}
                inputMode="url"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                placeholder="https://git.example.com"
                autoComplete="off"
                spellCheck={false}
                aria-invalid={!!errors.baseUrl}
                aria-describedby={errors.baseUrl ? 'git-base-url-error' : 'git-base-url-hint'}
              />
              {errors.baseUrl && (
                <span id="git-base-url-error" className={settings.fieldError} aria-live="polite">
                  {errors.baseUrl}
                </span>
              )}
              <span id="git-base-url-hint" className={ui.hint} hidden={!!errors.baseUrl}>
                Use https. Launchpad calls this host, so a private address needs an operator setting.
              </span>
            </div>
          ) : (
            <p>
              Address: <code>{info.hostedUrl}</code>
              {errors.baseUrl && (
                <span className={settings.fieldError} role="alert">
                  {' '}
                  {errors.baseUrl}
                </span>
              )}
            </p>
          )}
          <div className={ui.field}>
            <label htmlFor="git-name">Name (optional)</label>
            <input
              id="git-name"
              className={ui.input}
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="off"
              aria-invalid={!!errors.name}
              aria-describedby={errors.name ? 'git-name-error' : undefined}
            />
            {errors.name && (
              <span id="git-name-error" className={settings.fieldError} aria-live="polite">
                {errors.name}
              </span>
            )}
          </div>
          <div className={settings.actions}>
            <button type="submit" className={`${ui.btn} ${ui.primary}`}>
              Next
            </button>
            <button type="button" className={ui.btn} onClick={close}>
              Cancel
            </button>
          </div>
        </form>
      )}
      {step === 2 && (
        <form onSubmit={(e) => void submit(e)} noValidate aria-label="Mode and credentials">
          <fieldset className={settings.radios}>
            <legend>How should {info.label} reach Launchpad?</legend>
            <label>
              <input
                id="git-mode-token"
                type="radio"
                name="git-mode"
                checked={mode === 'token'}
                onChange={() => setMode('token')}
              />
              <span>Token: Launchpad makes the hook</span>
            </label>
            <label>
              <input
                type="radio"
                name="git-mode"
                checked={mode === 'webhook_only'}
                onChange={() => setMode('webhook_only')}
              />
              <span>Webhook only: I make the hook myself</span>
            </label>
          </fieldset>

          {mode === 'token' ? (
            <>
              <p>The token needs only these permissions. Use the line that fits the token form you see:</p>
              <ul className={styles.scopes}>
                {info.scopes.map((s) => (
                  <li key={s}>{s}</li>
                ))}
              </ul>
              <p className={ui.hint}>{info.scopeNote}</p>
              {safeAddress && info.tokenPage !== '' && (
                <p>
                  <a
                    href={`${address.trim().replace(/\/+$/, '')}${info.tokenPage}`}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    Make a token on {info.label} (opens in a new tab)
                  </a>
                </p>
              )}
              <div className={ui.field}>
                <label htmlFor="git-token">Token</label>
                <SecretInput
                  id="git-token"
                  value={token}
                  onChange={setToken}
                  invalid={!!errors.token}
                  describedBy={errors.token ? 'git-token-error' : 'git-token-hint'}
                />
                {errors.token && (
                  <span id="git-token-error" className={settings.fieldError} aria-live="polite">
                    {errors.token}
                  </span>
                )}
                <span id="git-token-hint" className={ui.hint} hidden={!!errors.token}>
                  Launchpad checks it, keeps it sealed and never shows it again.
                </span>
              </div>
            </>
          ) : (
            <>
              <p className={ui.hint}>
                Launchpad makes a secret and shows it once, with the address for the hook. You add the hook on{' '}
                {info.label} yourself.
              </p>
              {provider === 'gitlab' && (
                <div className={ui.field}>
                  <label htmlFor="git-pasted-secret">Use my own secret (optional)</label>
                  <SecretInput
                    id="git-pasted-secret"
                    value={pasted}
                    onChange={setPasted}
                    invalid={!!errors.secret}
                    describedBy={errors.secret ? 'git-pasted-secret-error' : 'git-pasted-secret-hint'}
                  />
                  {errors.secret && (
                    <span id="git-pasted-secret-error" className={settings.fieldError} aria-live="polite">
                      {errors.secret}
                    </span>
                  )}
                  <span id="git-pasted-secret-hint" className={ui.hint} hidden={!!errors.secret}>
                    For a hook that is already on GitLab. 32 to 256 characters, no space. Leave empty to get a
                    new secret.
                  </span>
                </div>
              )}
            </>
          )}

          {banner && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {banner}
            </p>
          )}
          {banner && duplicate && (
            <p>
              <Link to={existingPath} onClick={onClose}>
                Open the existing connection
              </Link>{' '}
              <span className={ui.hint}>
                There you can use Replace token, or press Remove connection to start again.
              </span>
            </p>
          )}
          <div className={settings.actions}>
            <button type="submit" className={`${ui.btn} ${ui.primary}`} aria-disabled={busy || undefined}>
              {busy ? (mode === 'token' ? 'Checking the token…' : 'Connecting…') : 'Connect'}
            </button>
            <button
              type="button"
              className={ui.btn}
              aria-disabled={busy || undefined}
              onClick={() => {
                if (busy) return;
                setStep(1);
                setBanner(null);
                setFocusId(`git-provider-${provider}`);
              }}
            >
              Back
            </button>
          </div>
        </form>
      )}
    </Dialog>
  );
}
