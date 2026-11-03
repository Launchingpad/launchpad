import { useEffect, useRef, useState, type FormEvent } from 'react';
import { usePushRelay, useRemovePushRelay, useSavePushRelay, useTestPush } from '../../api/queries';
import type { PushRelay, PushRelayInput } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { Spinner } from '../../ui/States';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import { PUSH_WORDS, pushErrorText, relaySecretProblem, relayUrlProblem } from './pushText';
import org from './organisation.module.css';
import styles from './push.module.css';
import ui from '../../styles/ui.module.css';

function hostOf(raw: string): string {
  try {
    return new URL(raw).hostname.toLowerCase().replace(/^\[|\]$/g, '');
  } catch {
    return '';
  }
}

function when(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

/**
 * Settings, Mobile push (LAUN-197, decisions-T35): the relay of the org. The relay URL and its secret are
 * write-only: the page shows that a relay is set and its host name, never either value.
 */
export function PushPage() {
  const relay = usePushRelay();
  return (
    <SettingsPage
      title="Mobile push"
      description="Wake the Launchpad mobile app when something needs a person. Launchpad holds no Apple or Google key: it sends a bare wake to a relay that you name, and the relay is run by the publisher of the app. A wake holds no title, comment or name. The app opens and fetches the real data with its own sign-in."
    >
      <div className={org.touch}>
        {relay.isPending ? (
          <Spinner label="Loading the mobile push settings" />
        ) : relay.isError ? (
          <FixedError
            title="Could not load the mobile push settings"
            error={relay.error}
            onRetry={() => void relay.refetch()}
          />
        ) : (
          <PushBody relay={relay.data} />
        )}
      </div>
    </SettingsPage>
  );
}

function PushBody({ relay }: { relay: PushRelay }) {
  const save = useSavePushRelay();
  const remove = useRemovePushRelay();
  const test = useTestPush();
  const [replacing, setReplacing] = useState(false);
  const [url, setUrl] = useState('');
  const [secret, setSecret] = useState('');
  const [urlProblem, setUrlProblem] = useState<string | null>(null);
  const [secretProblem, setSecretProblem] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [ok, setOk] = useState('');
  const [confirm, setConfirm] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; text: string } | null>(null);
  const urlRef = useRef<HTMLInputElement>(null);
  const secretRef = useRef<HTMLInputElement>(null);
  const replaceRef = useRef<HTMLButtonElement>(null);
  // Where focus goes once the form has swapped: the Replace button after Save or Keep, the URL field after Remove.
  const focusNext = useRef<'replace' | 'url' | null>(null);
  // Synchronous in-flight guards: two clicks in one tick would both pass an isPending check.
  const flying = useRef({ save: false, test: false, remove: false });

  const showFields = !relay.configured || replacing || relay.secretUnreadable;
  const busy = save.isPending || remove.isPending;
  const canStore = relay.available;
  // A new or unreadable relay needs both values; a replacement needs at least one.
  const needsBoth = !relay.configured || relay.secretUnreadable;

  useEffect(() => {
    // Not while the remove dialog is open: the page behind it is inert, and closing it restores focus.
    if (confirm) return;
    if (focusNext.current === 'replace' && !showFields && replaceRef.current) {
      focusNext.current = null;
      replaceRef.current.focus();
    } else if (focusNext.current === 'url' && showFields && urlRef.current) {
      focusNext.current = null;
      urlRef.current.focus();
    }
  });

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy || !canStore || flying.current.save || !showFields) return;
    setBanner(null);
    setOk('');
    const u = url.trim();
    const s = secret.trim();
    let uP: string | null = null;
    let sP: string | null = null;
    if (u === '' && (needsBoth || s === ''))
      uP = needsBoth ? 'Enter the relay URL.' : 'Enter a new URL or a new secret.';
    else if (u !== '') uP = relayUrlProblem(u);
    if (s === '' && needsBoth) sP = 'Enter the relay secret.';
    else if (s !== '') sP = relaySecretProblem(s);
    // A kept secret never goes to a new host: the server refuses it too.
    if (!sP && s === '' && !needsBoth && u !== '' && !uP && relay.host && hostOf(u) !== relay.host)
      sP = 'The relay host changed. Enter the secret again.';
    setUrlProblem(uP);
    setSecretProblem(sP);
    if (uP) {
      urlRef.current?.focus();
      return;
    }
    if (sP) {
      secretRef.current?.focus();
      return;
    }
    flying.current.save = true;
    try {
      const body: PushRelayInput = {
        ...(u !== '' ? { relayUrl: u } : {}),
        ...(s !== '' ? { relaySecret: s } : {}),
      };
      await save.mutateAsync(body);
      setUrl('');
      setSecret('');
      focusNext.current = 'replace';
      setReplacing(false);
      setTestResult(null);
      setOk('Saved.');
    } catch (err) {
      setBanner(pushErrorText(err, 'save'));
    } finally {
      flying.current.save = false;
    }
  }

  async function runTest() {
    if (flying.current.test) return;
    flying.current.test = true;
    setTestResult(null);
    try {
      const r = await test.mutateAsync();
      setTestResult({
        ok: true,
        text:
          r.sent === 1
            ? 'Test push sent to 1 of your devices.'
            : `Test push sent to ${r.sent} of your devices.`,
      });
    } catch (err) {
      setTestResult({ ok: false, text: pushErrorText(err, 'test') });
    } finally {
      flying.current.test = false;
    }
  }

  return (
    <>
      <form onSubmit={(e) => void submit(e)} noValidate aria-label="Mobile push relay">
        {!relay.available && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            This server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a relay. Ask the person who runs
            the server to set it.
          </p>
        )}
        {relay.secretUnreadable && relay.configured && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {PUSH_WORDS.push_unreadable}
          </p>
        )}
        {banner && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {banner}
          </p>
        )}

        <SettingsSection
          title="Relay"
          description="The relay URL and its secret are stored sealed and are never shown again; the page shows the host name only. The relay can be any public https host that you trust to hold your Apple and Google keys. Launchpad refuses private addresses: a numeric address in a private range is refused when you save, and a name that points to a private address is refused when a push is sent."
        >
          <SettingsCard padded>
            {relay.configured && (
              <div className={styles.state} data-testid="push-state">
                <span>
                  <strong>Set.</strong> Host <span className={styles.host}>{relay.host}</span>
                </span>
                <span className={org.hint}>
                  {relay.deviceCount} {relay.deviceCount === 1 ? 'device is' : 'devices are'} registered.
                </span>
                {relay.rateCappedCount > 0 && (
                  <span className={org.problem} role="status" data-testid="push-capped">
                    The hourly limit dropped {relay.rateCappedCount}{' '}
                    {relay.rateCappedCount === 1 ? 'push' : 'pushes'} in the last day.
                  </span>
                )}
                {relay.deadCount > 0 && (
                  <span className={org.problem} role="status">
                    {relay.deadCount} {relay.deadCount === 1 ? 'push' : 'pushes'} could not be delivered and{' '}
                    {relay.deadCount === 1 ? 'was' : 'were'} given up.
                  </span>
                )}
                {relay.lastError ? (
                  <span className={org.problem} role="status">
                    Last push failed. {PUSH_WORDS[relay.lastError] ?? 'The relay did not accept it.'}
                  </span>
                ) : relay.lastOkAt ? (
                  <span className={org.hint}>Last push worked on {when(relay.lastOkAt)}.</span>
                ) : (
                  <span className={org.hint}>Nothing has been pushed yet.</span>
                )}
              </div>
            )}
            {showFields ? (
              <div className={styles.fields} style={{ marginTop: relay.configured ? 12 : 0 }}>
                <div className={ui.field}>
                  <label htmlFor="push-url">{relay.configured ? 'New relay URL' : 'Relay URL'}</label>
                  <input
                    id="push-url"
                    ref={urlRef}
                    className={ui.input}
                    type="text"
                    inputMode="url"
                    autoComplete="off"
                    autoCapitalize="none"
                    spellCheck={false}
                    placeholder="https://relay.example.com"
                    value={url}
                    disabled={!canStore}
                    onChange={(e) => {
                      setUrl(e.target.value);
                      setUrlProblem(null);
                      setOk('');
                    }}
                    aria-invalid={urlProblem !== null}
                    aria-describedby={urlProblem ? 'push-url-problem' : 'push-url-hint'}
                  />
                  {urlProblem ? (
                    <span id="push-url-problem" role="alert" className={org.fieldError}>
                      {urlProblem}
                    </span>
                  ) : (
                    <span id="push-url-hint" className={org.hint}>
                      {relay.configured && !relay.secretUnreadable
                        ? 'Leave empty to keep the current URL. A different host needs the secret again. Only https:// is accepted.'
                        : 'Only https:// is accepted.'}
                    </span>
                  )}
                </div>
                <div className={ui.field}>
                  <label htmlFor="push-secret">
                    {relay.configured ? 'New relay secret' : 'Relay secret'}
                  </label>
                  <input
                    id="push-secret"
                    ref={secretRef}
                    className={`${ui.input} ${styles.secret}`}
                    type="password"
                    autoComplete="off"
                    autoCapitalize="none"
                    spellCheck={false}
                    value={secret}
                    disabled={!canStore}
                    onChange={(e) => {
                      setSecret(e.target.value);
                      setSecretProblem(null);
                      setOk('');
                    }}
                    aria-invalid={secretProblem !== null}
                    aria-describedby={secretProblem ? 'push-secret-problem' : 'push-secret-hint'}
                  />
                  {secretProblem ? (
                    <span id="push-secret-problem" role="alert" className={org.fieldError}>
                      {secretProblem}
                    </span>
                  ) : (
                    <span id="push-secret-hint" className={org.hint}>
                      {relay.configured && !relay.secretUnreadable
                        ? 'Leave empty to keep the current secret. The relay expects it as a bearer token.'
                        : 'The relay expects it as a bearer token.'}
                    </span>
                  )}
                </div>
                <div className={org.actions}>
                  <button
                    type="submit"
                    className={`${ui.btn} ${ui.primary}`}
                    aria-disabled={busy || !canStore || undefined}
                  >
                    {save.isPending ? 'Saving' : 'Save'}
                  </button>
                  {replacing && relay.configured && (
                    <button
                      type="button"
                      className={`${ui.btn} ${ui.ghost}`}
                      onClick={() => {
                        focusNext.current = 'replace';
                        setReplacing(false);
                        setUrl('');
                        setSecret('');
                        setUrlProblem(null);
                        setSecretProblem(null);
                      }}
                    >
                      Keep the current relay
                    </button>
                  )}
                  <p role="status" className={org.ok}>
                    {ok}
                  </p>
                </div>
              </div>
            ) : (
              <div className={org.actions} style={{ marginTop: 12 }}>
                <button
                  type="button"
                  ref={replaceRef}
                  className={ui.btn}
                  disabled={!canStore}
                  onClick={() => {
                    setReplacing(true);
                    setOk('');
                    setTimeout(() => urlRef.current?.focus(), 0);
                  }}
                >
                  Replace relay
                </button>
                <button type="button" className={`${ui.btn} ${ui.danger}`} onClick={() => setConfirm(true)}>
                  Remove
                </button>
                <p role="status" className={org.ok}>
                  {ok}
                </p>
              </div>
            )}
          </SettingsCard>
        </SettingsSection>

        <SettingsSection
          title="Test"
          description="Sends one wake to your own registered devices with the saved relay. Open the mobile app and sign in first."
        >
          <SettingsCard padded>
            <div className={org.actions}>
              <button
                type="button"
                className={ui.btn}
                aria-disabled={test.isPending || !relay.configured || relay.secretUnreadable || undefined}
                onClick={() => {
                  if (!test.isPending && relay.configured && !relay.secretUnreadable) void runTest();
                }}
              >
                {test.isPending ? 'Sending…' : 'Send test push'}
              </button>
            </div>
            <div role="status" aria-live="polite" className={styles.result} data-testid="push-test-result">
              {testResult && (
                <p className={testResult.ok ? undefined : `${ui.banner} ${ui.errorBanner}`}>
                  {testResult.text}
                </p>
              )}
            </div>
          </SettingsCard>
        </SettingsSection>
      </form>
      {confirm && (
        <RemoveDialog
          pending={remove.isPending}
          onClose={() => setConfirm(false)}
          onConfirm={async () => {
            if (flying.current.remove) return;
            flying.current.remove = true;
            try {
              await remove.mutateAsync();
              setConfirm(false);
              setOk('');
              setTestResult(null);
              // The dialog and the Remove button are gone: the effect above focuses the URL field of the
              // empty form once it is drawn, never the body.
              focusNext.current = 'url';
            } catch (err) {
              setConfirm(false);
              setBanner(pushErrorText(err, 'remove'));
            } finally {
              flying.current.remove = false;
            }
          }}
        />
      )}
    </>
  );
}

function RemoveDialog({
  pending,
  onClose,
  onConfirm,
}: {
  pending: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Dialog title="Remove the push relay?" width={460} onClose={onClose}>
      <p>
        This deletes the stored relay URL and secret. Pushes that are waiting are dropped. Registered devices
        stay, and push starts again when you set a relay.
      </p>
      <div className={org.actions} style={{ justifyContent: 'flex-end', marginTop: 12 }}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          aria-disabled={pending || undefined}
          onClick={() => !pending && onConfirm()}
        >
          {pending ? 'Removing…' : 'Remove relay'}
        </button>
      </div>
    </Dialog>
  );
}
