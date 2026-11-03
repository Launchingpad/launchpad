import { useEffect, useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import { useCreateToken } from '../../api/queries';
import type { CreatedToken } from '../../api/types';
import { waitText } from './tokenText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

// The rules are the server's for a token made in the web app (decisions-T11 #2-3; openapi TokenInput):
// a name of 1 to 200 characters, read or read and write, and an expiry of 1 to 365 days. The server stays the judge.
const MAX_DAYS = 365;
const NAME_RULE = 'Enter 1 to 200 characters.';
const DAYS_RULE = `Enter a whole number from 1 to ${MAX_DAYS}.`;
const DEFAULT_DAYS = { person: '90', agent: '365' };

export interface Owner {
  username: string;
  name: string;
  isAgent: boolean;
}

type Field = 'name' | 'days';
type Access = 'read' | 'write';

export function TokenForm({
  idPrefix,
  owners,
  initialOwner,
  lockOwner = false,
  autoFocus = false,
  onIssued,
  onCancel,
}: {
  /** Makes the field ids unique when the form is in a dialog over a page that has one too. */
  idPrefix: string;
  /** The first owner is the signed-in person; the others are agents. */
  owners: Owner[];
  initialOwner: string;
  lockOwner?: boolean;
  autoFocus?: boolean;
  /** Gets the 201 body, secret included. The caller keeps it in component state only. */
  onIssued: (created: CreatedToken) => void;
  onCancel?: () => void;
}) {
  const createToken = useCreateToken();
  const self = owners[0]!;
  const [owner, setOwner] = useState(initialOwner);
  const isAgent = owners.find((o) => o.username === owner)?.isAgent ?? false;
  const [name, setName] = useState('');
  // An agent exists to write and gets a year; a person gets read only and 90 days. A choice the person
  // made is kept when the owner changes.
  const [access, setAccess] = useState<Access | null>(null);
  const [days, setDays] = useState<string | null>(null);
  const accessValue: Access = access ?? (isAgent ? 'write' : 'read');
  const daysValue = days ?? DEFAULT_DAYS[isAgent ? 'agent' : 'person'];
  const [errors, setErrors] = useState<Partial<Record<Field, string>>>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [focusField, setFocusField] = useState<Field | null>(null);

  // Move focus after the error is on screen (state updates are batched, so the DOM is not ready in the handler).
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(`${idPrefix}-${focusField}`)?.focus();
    setFocusField(null);
  }, [focusField, idPrefix]);

  function fail(field: Field, text: string) {
    setErrors({ [field]: text });
    setFocusField(field);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    const n = name.trim();
    if (n.length < 1 || [...n].length > 200) return fail('name', NAME_RULE);
    const d = Number(daysValue);
    if (!/^\d+$/.test(daysValue.trim()) || d < 1 || d > MAX_DAYS) return fail('days', DAYS_RULE);
    setErrors({});
    setBusy(true);
    try {
      const created = await createToken({
        name: n,
        scopes: accessValue === 'write' ? ['read', 'write'] : ['read'],
        expiresInDays: d,
        ...(owner !== self.username ? { user: owner } : {}),
      });
      onIssued(created);
    } catch (err) {
      handleError(err);
    } finally {
      setBusy(false);
    }
  }

  // Server text never reaches the screen: a 400 maps to a field and its fixed rule, and every other
  // failure gets a fixed text. The 201 body is the only place the secret travels.
  function handleError(err: unknown) {
    if (!(err instanceof ApiError)) setBanner('Something went wrong. Try again.');
    else if (err.status === 400 && err.message.startsWith('name')) fail('name', NAME_RULE);
    else if (err.status === 400 && err.message.includes('expiresInDays')) fail('days', DAYS_RULE);
    else if (err.status === 400) setBanner('The server did not accept this token. Check the fields.');
    else if (err.status === 403) setBanner('You cannot make this token.');
    else if (err.status === 429) setBanner(`You made many tokens. Try again in ${waitText(err.retryAfter)}.`);
    else if (err.status === 0)
      setBanner(err.message); // fixed client text: could not reach the server
    else setBanner('Could not make the token. Try again.');
  }

  const id = (f: string) => `${idPrefix}-${f}`;
  const owner0 = owners.find((o) => o.username === owner) ?? self;
  return (
    <form onSubmit={(e) => void submit(e)} noValidate aria-label="Create a token">
      <div className={ui.field}>
        <label htmlFor={id('name')}>Token name</label>
        <input
          id={id('name')}
          className={ui.input}
          value={name}
          onChange={(e) => setName(e.target.value)}
          aria-invalid={!!errors.name}
          aria-describedby={errors.name ? id('name-error') : id('name-hint')}
          autoComplete="off"
          data-autofocus={autoFocus ? '' : undefined}
        />
        {errors.name && (
          <span id={id('name-error')} className={styles.fieldError} aria-live="polite">
            {errors.name}
          </span>
        )}
        <span id={id('name-hint')} className={ui.hint} hidden={!!errors.name}>
          A name to find the token again, for example mcp.
        </span>
      </div>

      <div className={ui.field}>
        {lockOwner || owners.length < 2 ? (
          <>
            <span className={ui.label}>Owner</span>
            <span>
              {owner0.name} (<code>{owner0.username}</code>)
            </span>
          </>
        ) : (
          <>
            <label htmlFor={id('owner')}>Owner</label>
            <select
              id={id('owner')}
              className={ui.select}
              value={owner}
              onChange={(e) => setOwner(e.target.value)}
            >
              {owners.map((o, i) => (
                <option key={o.username} value={o.username}>
                  {i === 0 ? `Me (${o.username})` : `${o.name} (${o.username}, agent)`}
                </option>
              ))}
            </select>
          </>
        )}
      </div>

      <fieldset className={styles.radios}>
        <legend>Access</legend>
        <label>
          <input
            type="radio"
            name={id('access')}
            checked={accessValue === 'read'}
            onChange={() => setAccess('read')}
          />
          <span>Read only</span>
        </label>
        <label>
          <input
            type="radio"
            name={id('access')}
            checked={accessValue === 'write'}
            onChange={() => setAccess('write')}
          />
          <span>Read and write</span>
        </label>
      </fieldset>

      <div className={ui.field}>
        <label htmlFor={id('days')}>Days until it expires</label>
        <input
          id={id('days')}
          className={`${ui.input} ${styles.num}`}
          inputMode="numeric"
          value={daysValue}
          onChange={(e) => setDays(e.target.value)}
          aria-invalid={!!errors.days}
          aria-describedby={errors.days ? id('days-error') : id('days-hint')}
          autoComplete="off"
        />
        {errors.days && (
          <span id={id('days-error')} className={styles.fieldError} aria-live="polite">
            {errors.days}
          </span>
        )}
        <span id={id('days-hint')} className={ui.hint} hidden={!!errors.days}>
          From 1 to {MAX_DAYS} days. A token made here always expires.
        </span>
      </div>

      {banner && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {banner}
        </p>
      )}
      <div className={styles.actions}>
        <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
          {busy ? 'Creating…' : 'Create token'}
        </button>
        {onCancel && (
          <button type="button" className={ui.btn} onClick={onCancel}>
            Cancel
          </button>
        )}
      </div>
    </form>
  );
}
