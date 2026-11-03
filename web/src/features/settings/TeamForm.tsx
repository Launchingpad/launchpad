import { SettingsCard, SettingsSection } from './SettingsParts';
import { useEffect, useState, type FormEvent } from 'react';
import { ApiError, errorMessage } from '../../api/errors';
import { useCreateTeam } from '../../api/queries';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

// The rules are the server's (openapi TeamInput). The server stays the judge.
const KEY_RE = /^[A-Z][A-Z0-9]{0,9}$/;

type Field = 'key' | 'name';
type Errors = Partial<Record<Field, string>>;

const KEY_RULE = 'Use 1 to 10 characters: A-Z and 0-9. Start with a letter.';
const NAME_RULE = 'Enter 1 to 200 characters.';

/** The "Create a team" form. Used by Settings, Teams and by the empty issues page. */
export function TeamForm() {
  const create = useCreateTeam();
  const [key, setKey] = useState('');
  const [name, setName] = useState('');
  const [errors, setErrors] = useState<Errors>({});
  const [banner, setBanner] = useState<string | null>(null);
  const [focusField, setFocusField] = useState<Field | null>(null);
  const [added, setAdded] = useState<string | null>(null);

  // Move focus after the error is on screen (state updates are batched, so the DOM is not ready in the handler).
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(`team-${focusField}`)?.focus();
    setFocusField(null);
  }, [focusField]);

  function fail(field: Field, text: string) {
    setErrors({ [field]: text });
    setFocusField(field);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    setAdded(null);
    const k = key.trim();
    const n = name.trim();
    if (!KEY_RE.test(k)) return fail('key', KEY_RULE);
    if (n.length < 1 || [...n].length > 200) return fail('name', NAME_RULE);
    setErrors({});
    try {
      await create.mutateAsync({ key: k, name: n });
      setKey('');
      setName('');
      setAdded(`Team ${k} is created.`);
    } catch (err) {
      if (!(err instanceof ApiError)) setBanner(errorMessage(err));
      else if (err.status === 409) fail('key', 'A team with this key exists already. Use another key.');
      else if (err.status === 400 && err.message.startsWith('key')) fail('key', KEY_RULE);
      else if (err.status === 400 && err.message.startsWith('name')) fail('name', NAME_RULE);
      else if (err.status === 403) setBanner('You need admin access to create a team.');
      else if (err.status === 0)
        setBanner('Could not reach the server. Check your connection and try again.');
      else if (err.status === 429) setBanner('Too many requests. Wait a moment and try again.');
      else setBanner('Could not create the team. Try again.');
    }
  }

  return (
    <SettingsSection title="Create a team" id="team-form-title">
      <SettingsCard padded>
        <form onSubmit={(e) => void submit(e)} noValidate aria-labelledby="team-form-title">
          <div className={styles.pair}>
            <div className={ui.field}>
              <label htmlFor="team-key">Team key</label>
              <input
                id="team-key"
                className={ui.input}
                value={key}
                onChange={(e) => setKey(e.target.value.toUpperCase())}
                aria-invalid={!!errors.key}
                aria-describedby={`team-key-hint${errors.key ? ' team-key-error' : ''}`}
                autoComplete="off"
                autoCapitalize="characters"
                spellCheck={false}
              />
              {errors.key && (
                <span id="team-key-error" className={styles.fieldError} aria-live="polite">
                  {errors.key}
                </span>
              )}
            </div>
            <div className={ui.field}>
              <label htmlFor="team-name">Team name</label>
              <input
                id="team-name"
                className={ui.input}
                value={name}
                onChange={(e) => setName(e.target.value)}
                aria-invalid={!!errors.name}
                aria-describedby={errors.name ? 'team-name-error' : undefined}
                autoComplete="off"
              />
              {errors.name && (
                <span id="team-name-error" className={styles.fieldError} aria-live="polite">
                  {errors.name}
                </span>
              )}
            </div>
          </div>
          <p id="team-key-hint" className={ui.hint}>
            The key starts every issue number, for example ORB-12.
          </p>
          {banner && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {banner}
            </p>
          )}
          <div className={styles.actions}>
            <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={create.isPending}>
              {create.isPending ? 'Creating…' : 'Create team'}
            </button>
            {/* Always in the page, so a screen reader sees the text arrive and reads it. */}
            <span role="status" className={ui.hint}>
              {added}
            </span>
          </div>
        </form>
      </SettingsCard>
    </SettingsSection>
  );
}
