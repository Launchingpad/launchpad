import { useId, useRef, useState, type FormEvent } from 'react';
import { useBackupRun, useDestinationActions } from '../../../api/backups';
import type {
  BackupDestination,
  BackupDestinationType,
  CreateBackupDestinationInput,
  UpdateBackupDestinationInput,
} from '../../../api/types';
import { Dialog } from '../../../ui/Dialog';
import { backupErrorText, FIRST_PIN_NEEDS_SECRETS, PageBlocked, WEEKDAYS } from './backupText';
import { CheckPanel } from './CheckPanel';
import {
  buildConfig,
  configChanged,
  configProblem,
  configToForm,
  NAME_MAX,
  scheduleProblem,
  TYPES,
  type FormConfig,
  type TypeDef,
} from './destinationTypes';
import { isEntered, NO_EDIT, SecretInput, type SecretEdit } from './SecretInput';
import { PollError, RunBox } from './StepList';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

type SecretMap = Record<string, SecretEdit>;

function Field({
  label,
  value,
  onChange,
  hint,
  options,
  inputMode,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  hint?: string;
  options?: { value: string; label: string }[];
  inputMode?: 'numeric';
}) {
  const id = useId();
  return (
    <div className={ui.field}>
      <label htmlFor={id}>{label}</label>
      {options ? (
        <select id={id} className={ui.select} value={value} onChange={(e) => onChange(e.target.value)}>
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          className={ui.input}
          value={value}
          inputMode={inputMode}
          onChange={(e) => onChange(e.target.value)}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          aria-describedby={hint ? `${id}-h` : undefined}
        />
      )}
      {hint && (
        <span id={`${id}-h`} className={ui.hint}>
          {hint}
        </span>
      )}
    </div>
  );
}

/** The secrets of the form that carry a value or a clear, as a request map. */
function typedSecrets(def: TypeDef, secrets: SecretMap): Record<string, string> {
  const out: Record<string, string> = {};
  for (const s of def.secrets) {
    const e = secrets[s.name];
    if (!e) continue;
    if (e.value !== '') out[s.name] = e.value;
    else if (e.clear) out[s.name] = '';
  }
  return out;
}

export function DestinationDialog({
  editId,
  destinations,
  supportedTypes,
  onClose,
  onSaved,
}: {
  editId: string | null;
  destinations: BackupDestination[];
  supportedTypes: BackupDestinationType[];
  onClose: () => void;
  onSaved: (text: string) => void;
}) {
  const saved = editId ? destinations.find((d) => d.id === editId) : undefined;
  if (editId && !saved) return null;
  return (
    <Dialog title={saved ? `Edit ${saved.name}` : 'Add a destination'} onClose={onClose} width={640}>
      <DestinationForm saved={saved} supportedTypes={supportedTypes} onClose={onClose} onSaved={onSaved} />
    </Dialog>
  );
}

function DestinationForm({
  saved,
  supportedTypes,
  onClose,
  onSaved,
}: {
  saved: BackupDestination | undefined;
  supportedTypes: BackupDestinationType[];
  onClose: () => void;
  onSaved: (text: string) => void;
}) {
  const actions = useDestinationActions();
  const idp = useId();
  const [type, setType] = useState<BackupDestinationType>(saved?.type ?? supportedTypes[0] ?? 's3');
  const def = TYPES[type];
  const init = configToForm(def, saved?.config);
  const [name, setName] = useState(saved?.name ?? '');
  const [values, setValues] = useState<FormConfig>(init.values);
  const [pin, setPin] = useState(init.pin);
  const [secrets, setSecrets] = useState<SecretMap>({});
  const [secretEdits, setSecretEdits] = useState(0); // counts secret edits: the Test result belongs to one set
  const [schedule, setSchedule] = useState<'daily' | 'weekly'>(saved?.schedule ?? 'daily');
  const [atUtc, setAtUtc] = useState(saved?.atUtc ?? '04:15');
  const [weekday, setWeekday] = useState(saved?.weekday ?? 1);
  const [keepLast, setKeepLast] = useState(String(saved?.keepLast ?? 14));
  const [verify, setVerify] = useState(String(saved?.verifyEveryDays ?? 7));
  const [confirmLow, setConfirmLow] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [code, setCode] = useState('');
  const [exchangeId, setExchangeId] = useState<string | null>(null);
  const [exchangeError, setExchangeError] = useState<string | null>(null);
  const exchangeRun = useBackupRun(exchangeId);
  const exchanging = useRef(false);
  const [exchangeBusy, setExchangeBusy] = useState(false);

  function switchType(t: BackupDestinationType) {
    setType(t);
    const f = configToForm(TYPES[t]);
    setValues(f.values);
    setPin('');
    setSecrets({}); // a secret typed for one type is not kept for another
    setSecretEdits((c) => c + 1);
  }

  const storedNames = (saved?.secretsSet ?? []).filter((n) => def.secrets.some((s) => s.name === n));
  const hasRefresh = (saved?.secretsSet ?? []).includes('refreshToken');
  const changed = saved ? configChanged(def, saved, values, pin) : false;
  const missing = saved && changed ? storedNames.filter((n) => !isEntered(secrets[n])) : [];
  const missingLabels = missing.map((n) => def.secrets.find((s) => s.name === n)?.label ?? n);
  const anyTyped = def.secrets.some((s) => isEntered(secrets[s.name]));

  function setSecret(n: string, e: SecretEdit) {
    setSecrets((m) => ({ ...m, [n]: e }));
    setSecretEdits((c) => c + 1);
  }

  const lowKeep = keepLast.trim() === '1' && (!saved || saved.keepLast !== 1);

  function problem(): string | null {
    if (!name.trim()) return 'Name is required.';
    if ([...name.trim()].length > NAME_MAX) return `Name can have at most ${NAME_MAX} characters.`;
    const c = configProblem(def, values);
    if (c) return c;
    const s = scheduleProblem({ schedule, atUtc, weekday, keepLast, verifyEveryDays: verify });
    if (s) return s;
    if (lowKeep && !confirmLow)
      return 'Keeping only 1 backup is risky: a bad backup replaces your only good one. Tick the box to confirm.';
    if (!saved) {
      for (const n of def.requiredSecrets) {
        if (!(secrets[n]?.value ?? ''))
          return `${def.secrets.find((x) => x.name === n)?.label ?? n} is required.`;
      }
      if (def.oneOfSecrets && !def.oneOfSecrets.some((n) => (secrets[n]?.value ?? '') !== ''))
        return 'Enter a password or a private key.';
    }
    if (missing.length)
      return `You changed the connection settings. Enter these secrets again, or clear them: ${missingLabels.join(', ')}.`;
    return null;
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    const p = problem();
    if (p) {
      setError(p);
      return;
    }
    setBusy(true);
    try {
      if (!saved) {
        const input: CreateBackupDestinationInput = {
          name: name.trim(),
          type,
          config: buildConfig(def, values, pin),
          schedule,
          atUtc: atUtc.trim(),
          keepLast: Number(keepLast),
          verifyEveryDays: Number(verify),
          ...(schedule === 'weekly' ? { weekday } : {}),
          ...(Number(keepLast) < 2 ? { confirmLowKeep: true } : {}),
        };
        const sec = typedSecrets(def, secrets);
        if (Object.keys(sec).length) input.secrets = sec;
        const made = await actions.create(input);
        setSecrets({}); // the secrets leave the page at once
        onSaved(
          type === 'dropbox'
            ? `${made.name} is saved and disabled. Open it to authorise the Dropbox app.`
            : `${made.name} is saved and disabled. Run a test, then enable it.`,
        );
        return;
      }
      const body: UpdateBackupDestinationInput = {};
      if (name.trim() !== saved.name) body.name = name.trim();
      if (changed) body.config = buildConfig(def, values, pin, saved.config);
      const sec: Record<string, string> = typedSecrets(def, secrets);
      // A Dropbox authorisation belongs to the app key: a changed connection clears it (it can only be cleared here).
      if (changed && hasRefresh && type === 'dropbox') sec.refreshToken = '';
      if (Object.keys(sec).length) body.secrets = sec;
      if (schedule !== saved.schedule) body.schedule = schedule;
      if (atUtc.trim() !== saved.atUtc) body.atUtc = atUtc.trim();
      if (schedule === 'weekly' && weekday !== saved.weekday) body.weekday = weekday;
      if (schedule === 'daily' && saved.weekday !== null) body.weekday = null;
      if (Number(keepLast) !== saved.keepLast) {
        body.keepLast = Number(keepLast);
        if (Number(keepLast) < 2) body.confirmLowKeep = true;
      }
      if (Number(verify) !== saved.verifyEveryDays) body.verifyEveryDays = Number(verify);
      const made = await actions.update(saved.id, body);
      setSecrets({});
      onSaved(`${made.name} is saved.`);
    } catch (err) {
      setError(backupErrorText(err, 'destination'));
    } finally {
      setBusy(false);
    }
  }

  // ----- probe and test -----

  const cfgNow = (extraPin = '') => buildConfig(def, values, pin.trim() || extraPin, saved?.config);
  const formProblem = configProblem(def, values);
  const probeBlocked = formProblem ? 'Fill in the connection fields first.' : null;
  let testBlocked: string | null = probeBlocked;
  if (!testBlocked && !def.unsavedTest && !saved)
    testBlocked = 'A Dropbox destination can be tested after it is saved and authorised.';
  if (!testBlocked && !saved) {
    const need = def.requiredSecrets.find((n) => !(secrets[n]?.value ?? ''));
    if (need) testBlocked = 'Enter the secrets first. The test signs in.';
    else if (def.oneOfSecrets && !def.oneOfSecrets.some((n) => (secrets[n]?.value ?? '') !== ''))
      testBlocked = 'Enter a password or a private key first. The test signs in.';
  }
  if (!testBlocked && saved && !def.unsavedTest && (changed || anyTyped))
    testBlocked = 'Save your changes first. A Dropbox destination is tested as it is saved.';
  if (!testBlocked && saved && changed && missing.length)
    testBlocked = `You changed the connection settings. Enter these secrets again to test them: ${missingLabels.join(', ')}.`;

  async function probe() {
    if (saved && !changed) return actions.probeSaved(saved.id);
    return actions.probeUnsaved({ type, config: cfgNow() });
  }

  async function runTest(confirmed: string | undefined) {
    if (saved) {
      const savedPin =
        typeof saved.config[def.anchor] === 'string' ? (saved.config[def.anchor] as string) : '';
      const firstPin = !!confirmed && pin.trim() === '' && savedPin === '';
      const sameAsSaved = !configChanged(def, saved, values, pin) && !anyTyped;
      if (sameAsSaved || (firstPin && !configChanged(def, saved, values, '') && !anyTyped)) {
        // The server saves a first pin before it queues the run (decisions #36).
        const run = await actions.testSaved(saved.id, firstPin ? confirmed : undefined);
        if (firstPin) setPin(confirmed);
        return run;
      }
    }
    // A first pin with a retyped secret cannot reuse the other saved secrets: the server uses them only with the
    // saved config, and the pin is new. Ask for them again instead of sending a test the server will refuse.
    if (saved && confirmed && !pin.trim() && storedNames.some((n) => !isEntered(secrets[n])))
      throw new PageBlocked(FIRST_PIN_NEEDS_SECRETS);
    const usePin = pin.trim() || confirmed || '';
    if (confirmed && !pin.trim()) setPin(confirmed);
    const config = buildConfig(def, values, usePin, saved?.config);
    const stillSame = saved ? !configChanged(def, saved, values, usePin) : false;
    return actions.testUnsaved({
      type,
      config,
      secrets: typedSecrets(def, secrets),
      ...(saved && stillSame ? { useSavedSecretsFrom: saved.id } : {}),
      ...(confirmed ? { confirmedFingerprint: confirmed } : {}),
    });
  }

  async function exchange() {
    if (!saved || exchanging.current) return;
    setExchangeError(null);
    const c = code.trim();
    if (!c) {
      setExchangeError('Paste the authorisation code first.');
      return;
    }
    exchanging.current = true;
    setExchangeBusy(true);
    setCode(''); // the code is single use; it leaves the page before the request
    try {
      const run = await actions.exchange(saved.id, c);
      setExchangeId(run.id);
    } catch (err) {
      setExchangeError(backupErrorText(err, 'check'));
      setCode(c); // the server did not take it: the admin can send it again
    } finally {
      exchanging.current = false;
      setExchangeBusy(false);
    }
  }

  return (
    <form
      onSubmit={(e) => void submit(e)}
      noValidate
      aria-label={saved ? 'Edit destination' : 'New destination'}
    >
      <div className={ui.field}>
        <label htmlFor={`${idp}-type`}>Type</label>
        <select
          id={`${idp}-type`}
          className={ui.select}
          value={type}
          disabled={!!saved}
          onChange={(e) => switchType(e.target.value as BackupDestinationType)}
        >
          {(saved ? [saved.type] : supportedTypes).map((t) => (
            <option key={t} value={t}>
              {TYPES[t].label}
            </option>
          ))}
        </select>
        <span className={ui.hint}>{def.summary}</span>
      </div>
      {saved && !saved.typeSupported && (
        <p className={`${ui.banner} ${ui.errorBanner}`}>
          This server has no driver for this type, so it cannot run backups here.
        </p>
      )}
      <Field label="Name" value={name} onChange={setName} hint="Shown in the list and in the mail." />

      <h3 className={styles.formTitle}>Connection</h3>
      {def.config.map((f) => (
        <Field
          key={f.name}
          label={f.label}
          value={values[f.name] ?? ''}
          options={f.options}
          hint={f.hint}
          inputMode={f.number ? 'numeric' : undefined}
          onChange={(v) => setValues((m) => ({ ...m, [f.name]: v }))}
        />
      ))}
      <Field
        label={def.anchorLabel}
        value={pin}
        onChange={setPin}
        hint={
          def.anchor === 'tlsPin'
            ? 'The probe fills this in when the certificate is not trusted. Empty means the normal certificate check. A pin replaces the chain and host name check. The expiry date is still checked.'
            : 'The probe fills this in. The server must show this key at every backup.'
        }
      />

      <h3 className={styles.formTitle}>Secrets</h3>
      <p className={ui.hint}>
        A secret is write-only: the page never shows a saved one. Use a key limited to one folder or bucket,
        with write, list and delete rights.
        {saved && ' If you change a connection setting, the server needs every stored secret again.'}
      </p>
      {def.secrets.map((s) => (
        <SecretInput
          key={`${type}-${s.name}`}
          label={s.label}
          isSet={storedNames.includes(s.name)}
          edit={secrets[s.name] ?? NO_EDIT}
          onChange={(e) => setSecret(s.name, e)}
          multiline={s.multiline}
          hint={s.hint}
          mustReenter={changed && storedNames.includes(s.name)}
        />
      ))}
      {saved && changed && missing.length > 0 && (
        <p role="alert" className={`${ui.banner}`} data-testid="reenter-warning">
          You changed the connection settings, so the server needs these secrets again:{' '}
          {missingLabels.join(', ')}. A saved secret must not go to a new host by an edit that does not enter
          it again.
        </p>
      )}

      <h3 className={styles.formTitle}>Schedule and keep</h3>
      <div className={styles.pair}>
        <div className={ui.field}>
          <label htmlFor={`${idp}-sched`}>Schedule</label>
          <select
            id={`${idp}-sched`}
            className={ui.select}
            value={schedule}
            onChange={(e) => setSchedule(e.target.value as 'daily' | 'weekly')}
          >
            <option value="daily">Every day</option>
            <option value="weekly">Every week</option>
          </select>
        </div>
        <Field label="Time (UTC, HH:MM)" value={atUtc} onChange={setAtUtc} />
      </div>
      {schedule === 'weekly' && (
        <div className={ui.field}>
          <label htmlFor={`${idp}-wd`}>Day of the week</label>
          <select
            id={`${idp}-wd`}
            className={ui.select}
            value={weekday}
            onChange={(e) => setWeekday(Number(e.target.value))}
          >
            {WEEKDAYS.map((d, i) => (
              <option key={d} value={i}>
                {d}
              </option>
            ))}
          </select>
        </div>
      )}
      <div className={styles.pair}>
        <Field
          label="Keep the newest"
          value={keepLast}
          onChange={setKeepLast}
          inputMode="numeric"
          hint="Backups kept, 1 to 365."
        />
        <Field
          label="Verify every (days)"
          value={verify}
          onChange={setVerify}
          inputMode="numeric"
          hint="0 to 90. 0 turns the check off."
        />
      </div>
      {lowKeep && (
        <label className={styles.check1}>
          <input type="checkbox" checked={confirmLow} onChange={(e) => setConfirmLow(e.target.checked)} />
          <span>I know that keeping only 1 backup is risky.</span>
        </label>
      )}

      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      <div className={styles.rowActions}>
        <button
          type="submit"
          className={`${ui.btn} ${ui.primary}`}
          disabled={busy || (!!saved && missing.length > 0)}
        >
          {busy ? 'Saving…' : saved ? 'Save changes' : 'Save destination'}
        </button>
        <button type="button" className={ui.btn} onClick={onClose}>
          Cancel
        </button>
      </div>

      {type === 'dropbox' && saved && (
        <div className={styles.check}>
          <h3 className={styles.checkTitle}>Authorise Dropbox</h3>
          {hasRefresh ? (
            <p className={styles.okText}>Authorised. A refresh token is stored.</p>
          ) : (
            <p className={ui.hint}>Not authorised yet.</p>
          )}
          <ol className={styles.checkIntro}>
            <li>
              Make a Dropbox app with the &quot;App folder&quot; permission. Save its app key and app secret
              above.
            </li>
            <li>
              Open
              https://www.dropbox.com/oauth2/authorize?client_id=APP_KEY&amp;token_access_type=offline&amp;response_type=code
              in your browser. Replace APP_KEY with your app key.
            </li>
            <li>Allow the app. Paste the code that Dropbox shows here. A code works once.</li>
          </ol>
          <SecretInputCode value={code} onChange={setCode} onEnter={() => void exchange()} />
          <div className={styles.rowActions}>
            <button type="button" className={ui.btn} disabled={exchangeBusy} onClick={() => void exchange()}>
              {exchangeBusy ? 'Sending…' : hasRefresh ? 'Authorise again' : 'Authorise'}
            </button>
          </div>
          {exchangeError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {exchangeError}
            </p>
          )}
          <RunBox run={exchangeRun.data} label="Authorise" />
          <PollError query={exchangeRun} />
        </div>
      )}

      <CheckPanel
        key={`${type}\u0000${def.config.map((f) => (values[f.name] ?? '').trim()).join('\u0000')}`}
        anchorLabel={def.anchorLabel}
        pinText={pin}
        testResetKey={secretEdits}
        probeBlocked={probeBlocked}
        testBlocked={testBlocked}
        probe={probe}
        test={runTest}
      />
    </form>
  );
}

function SecretInputCode({
  value,
  onChange,
  onEnter,
}: {
  value: string;
  onChange: (v: string) => void;
  onEnter: () => void;
}) {
  const id = useId();
  return (
    <div className={ui.field}>
      <label htmlFor={id}>Authorisation code</label>
      <input
        id={id}
        type="password"
        className={ui.input}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            onEnter();
          }
        }}
        autoComplete="off"
        spellCheck={false}
      />
      <span className={ui.hint}>The code is not stored in the browser. It goes to the server once.</span>
    </div>
  );
}
