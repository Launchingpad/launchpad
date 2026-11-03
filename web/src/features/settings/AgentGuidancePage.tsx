import { useEffect, useRef, useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import { useAgentGuidance, useClearAgentGuidance, useSaveAgentGuidance, useTeams } from '../../api/queries';
import { Spinner } from '../../ui/States';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import styles from './organisation.module.css';
import ui from '../../styles/ui.module.css';

/** The server cap: UTF-8 bytes, not characters. */
export const GUIDANCE_MAX_BYTES = 8192;

const bytesOf = (s: string) => new TextEncoder().encode(s).length;

/** A fixed sentence for a failed save or remove. No server text reaches the screen. */
export function guidanceErrorText(err: unknown, action: 'save' | 'remove' = 'save'): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return `The server did not accept this text. Use 1 to ${GUIDANCE_MAX_BYTES} bytes, with no bidirectional or zero-width characters.`;
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You need admin access to change the guidance.';
    case 404:
      return 'The server cannot find the team. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return action === 'remove'
        ? 'Could not remove the guidance. Try again.'
        : 'Could not save the guidance. Try again.';
  }
}

/** Settings, Agent guidance (LAUN-200): text for the agents of the org, org-wide and per team. */
export function AgentGuidancePage() {
  const guidance = useAgentGuidance();
  const teams = useTeams();
  // The card the person last worked in: any other card drops its Saved. status.
  const [active, setActive] = useState('');
  return (
    <SettingsPage
      title="Agent guidance"
      description="Working conventions for the agents of this organisation. An agent reads them with the get_guidance tool at the start of work. They are advice: they never give an agent more access."
    >
      <div className={styles.touch}>
        {guidance.isPending ? (
          <Spinner label="Loading the guidance" />
        ) : guidance.isError ? (
          <FixedError
            title="Could not load the guidance"
            error={guidance.error}
            onRetry={() => void guidance.refetch()}
          />
        ) : (
          <>
            <SettingsSection
              title="Organisation"
              description="Every agent gets this text, whatever team it works in."
            >
              <EditorBody
                team={null}
                active={active}
                onActive={setActive}
                label="Organisation guidance"
                saved={guidance.data.items.find((e) => e.team === null)}
              />
            </SettingsSection>
            <SettingsSection
              title="Teams"
              description="Added after the organisation text, for agents that work in this team."
            >
              {(teams.data ?? []).map((t) => (
                <EditorBody
                  key={t.id}
                  team={t.key}
                  active={active}
                  onActive={setActive}
                  label={`${t.key} guidance`}
                  saved={guidance.data.items.find((e) => e.team === t.key)}
                />
              ))}
            </SettingsSection>
          </>
        )}
      </div>
    </SettingsPage>
  );
}

function EditorBody({
  team,
  label,
  active,
  onActive,
  saved: savedEntry,
}: {
  team: string | null;
  label: string;
  active: string;
  onActive: (id: string) => void;
  saved: { text: string; updatedAt: string } | undefined;
}) {
  const saved = savedEntry?.text ?? '';
  const save = useSaveAgentGuidance();
  const clear = useClearAgentGuidance();
  const [text, setText] = useState(saved);
  // A change from outside (another admin) fills the editor, unless the person has edited it.
  const synced = useRef(saved);
  useEffect(() => {
    if (saved === synced.current) return;
    if (text === synced.current) setText(saved);
    synced.current = saved;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only a new server value may run this
  }, [saved]);
  const [banner, setBanner] = useState<string | null>(null);
  const [ok, setOk] = useState('');
  const size = bytesOf(text);
  const tooBig = size > GUIDANCE_MAX_BYTES;
  const busy = save.isPending || clear.isPending;
  const id = `guidance-${team ?? 'org'}`;
  useEffect(() => {
    if (active !== id) setOk('');
  }, [active, id]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    onActive(id);
    setBanner(null);
    setOk('');
    const t = text.trim();
    if (t === '') {
      setBanner('Write some text, or use Remove.');
      return;
    }
    if (tooBig) {
      setBanner(`The text is over ${GUIDANCE_MAX_BYTES} bytes. Make it shorter.`);
      return;
    }
    if (t === saved) {
      setBanner('Nothing changed.');
      return;
    }
    try {
      await save.mutateAsync({ team, text: t });
      synced.current = t;
      setText(t);
      setOk('Saved.');
    } catch (err) {
      setBanner(guidanceErrorText(err, 'save'));
    }
  }

  async function remove() {
    if (busy) return;
    onActive(id);
    setBanner(null);
    setOk('');
    try {
      await clear.mutateAsync(team);
      synced.current = '';
      setText('');
      setOk('Removed.');
    } catch (err) {
      setBanner(guidanceErrorText(err, 'remove'));
    }
  }

  return (
    <SettingsCard padded>
      <form onSubmit={(e) => void submit(e)} noValidate aria-label={`${label} form`}>
        {banner && (
          <p role="alert" className={styles.problem}>
            {banner}
          </p>
        )}
        <div className={ui.field}>
          <label htmlFor={id}>{label}</label>
          <textarea
            id={id}
            className={ui.textarea}
            rows={6}
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              setBanner(null);
              onActive(id);
            }}
            aria-describedby={`${id}-hint`}
            aria-invalid={tooBig}
          />
          <span id={`${id}-hint`} className={styles.hint}>
            Markdown. {size} of {GUIDANCE_MAX_BYTES} bytes.
          </span>
        </div>
        <div className={styles.actions}>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} aria-disabled={busy || undefined}>
            {save.isPending ? 'Saving' : 'Save'}
          </button>
          <button
            type="button"
            className={`${ui.btn} ${ui.danger}`}
            aria-disabled={busy || saved === '' || undefined}
            onClick={() => saved !== '' && void remove()}
          >
            Remove
          </button>
          <p role="status" className={styles.ok}>
            {ok}
          </p>
        </div>
      </form>
    </SettingsCard>
  );
}
