import { useEffect, useRef, useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import {
  useCreatePersonalWebhook,
  usePersonalWebhooks,
  useRemovePersonalWebhook,
  useTestPersonalWebhook,
  useUpdatePersonalWebhook,
} from '../../api/queries';
import type {
  PersonalWebhook,
  PersonalWebhookCreate,
  PersonalWebhookList,
  PersonalWebhookPatch,
} from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, Spinner } from '../../ui/States';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import { CHAT_KINDS, CHAT_MAX_NAME, CHAT_WORDS, kindOf, type ChatKind } from './chatText';
import { PERSONAL_EVENTS, PERSONAL_LIMIT_FALLBACK, personalErrorText } from './personalWebhookText';
import org from './organisation.module.css';
import styles from './chatServices.module.css';
import ui from '../../styles/ui.module.css';

function when(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

/**
 * Settings, Personal webhooks (LAUN-229, decisions-T40): up to three Slack, Discord, Fluxer or custom webhooks
 * of the signed-in member. They post when an issue is assigned to the member, or when an issue the member
 * owns changes. Only the member sees them. A webhook URL is a secret and is write-only: the page shows the
 * host name, never the URL.
 */
export function PersonalWebhooksPage() {
  const list = usePersonalWebhooks();
  const forbidden = list.error instanceof ApiError && list.error.status === 403;
  return (
    <SettingsPage
      title="Personal webhooks"
      description="Get a post in your own Slack, Discord, Fluxer or other webhook when an issue is assigned to you, or when an issue you own changes. Only you see these webhooks. A post says what happened and links to Launchpad. It never holds a title, a comment or a person's name."
    >
      <div className={org.touch}>
        {list.isPending ? (
          <Spinner label="Loading your webhooks" />
        ) : forbidden ? (
          <EmptyState title="No personal webhooks for this account">
            <p>{personalErrorText(list.error, 'load')}</p>
          </EmptyState>
        ) : list.isError ? (
          <FixedError
            title="Could not load your webhooks"
            error={list.error}
            onRetry={() => void list.refetch()}
          />
        ) : (
          <Body data={list.data} />
        )}
      </div>
    </SettingsPage>
  );
}

type Dlg = { mode: 'create' } | { mode: 'edit'; id: string } | null;

function statusOf(s: PersonalWebhook): { text: string; tone: 'ok' | 'fail' | 'warn' | 'off' | 'plain' } {
  if (!s.enabled) return { text: 'Off', tone: 'off' };
  if (s.secretUnreadable) return { text: 'Webhook unreadable', tone: 'warn' };
  if (s.lastError) return { text: 'Last post failed', tone: 'fail' };
  if (s.lastOkAt) return { text: 'Working', tone: 'ok' };
  return { text: 'Not used yet', tone: 'plain' };
}

function eventsText(events: string[]): string {
  if (events.length === 0) return 'No choice is ticked, so nothing is posted.';
  return `Posts for: ${PERSONAL_EVENTS.filter((e) => events.includes(e.value))
    .map((e) => e.label.toLowerCase())
    .join(' and ')}.`;
}

function Body({ data }: { data: PersonalWebhookList }) {
  const remove = useRemovePersonalWebhook();
  const test = useTestPersonalWebhook();
  const [dlg, setDlg] = useState<Dlg>(null);
  const [confirm, setConfirm] = useState<PersonalWebhook | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [results, setResults] = useState<Record<string, { ok: boolean; text: string }>>({});
  const [testing, setTesting] = useState<string | null>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const addRef = useRef<HTMLButtonElement>(null);
  // Synchronous in-flight guards: two clicks in one tick would both pass an isPending check.
  const flying = useRef({ test: false, remove: false });
  // Where focus goes once a dialog has closed and its opener is gone (a removed row).
  const pendingFocus = useRef<{ row: string | null; removed?: string } | null>(null);
  const limit = data.limit || PERSONAL_LIMIT_FALLBACK;
  const full = data.items.length >= limit;

  useEffect(() => {
    const p = pendingFocus.current;
    if (dlg || confirm || !p) return;
    if (p.removed && data.items.some((x) => x.id === p.removed)) return;
    pendingFocus.current = null;
    const a = document.activeElement;
    if (a && a !== document.body && a.isConnected) return;
    const row = (id: string | null) =>
      id ? listRef.current?.querySelector<HTMLElement>(`[data-edit="${id}"]`) : null;
    const target = row(p.row) ?? addRef.current ?? document.querySelector<HTMLElement>('h1');
    target?.focus();
  }, [dlg, confirm, data.items]);

  async function runTest(s: PersonalWebhook) {
    if (flying.current.test) return;
    flying.current.test = true;
    setTesting(s.id);
    setResults((r) => {
      const { [s.id]: _gone, ...rest } = r;
      return rest;
    });
    try {
      await test.mutateAsync(s.id);
      setResults((r) => ({ ...r, [s.id]: { ok: true, text: 'Test message sent. Check the channel.' } }));
    } catch (err) {
      setResults((r) => ({ ...r, [s.id]: { ok: false, text: personalErrorText(err, 'test', s.kind) } }));
    } finally {
      flying.current.test = false;
      setTesting(null);
    }
  }

  async function doRemove(s: PersonalWebhook) {
    if (flying.current.remove) return;
    flying.current.remove = true;
    const rows = data.items;
    const i = rows.findIndex((r) => r.id === s.id);
    pendingFocus.current = { row: (rows[i + 1] ?? rows[i - 1])?.id ?? null, removed: s.id };
    setBanner(null);
    try {
      await remove.mutateAsync(s.id);
    } catch (err) {
      setBanner(personalErrorText(err, 'remove', s.kind));
    } finally {
      flying.current.remove = false;
      setConfirm(null);
    }
  }

  const editing = dlg?.mode === 'edit' ? data.items.find((s) => s.id === dlg.id) : undefined;

  return (
    <>
      {!data.available && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          This server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a webhook. Ask the person who runs
          the server to set it.
        </p>
      )}
      {data.available && !data.linksAvailable && (
        <p role="status" className={styles.warn}>
          PUBLIC_BASE_URL is not set on this server. Every post is a link, so no post is sent until it is set.
        </p>
      )}
      {banner && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {banner}
        </p>
      )}

      <SettingsSection
        title="Your webhooks"
        description="An issue you own is one you are assigned to or one you created. Nothing is posted for a change you make yourself, for an issue you muted, or for an issue of a team you cannot see. Your email and inbox choices under Notifications do not change these posts."
      >
        <SettingsCard>
          {data.items.length === 0 ? (
            <p className={styles.empty}>No personal webhook yet. Add one to get posts about your issues.</p>
          ) : (
            <ul className={styles.list} ref={listRef} aria-label="Personal webhooks">
              {data.items.map((s) => (
                <Row
                  key={s.id}
                  s={s}
                  result={results[s.id]}
                  testing={testing === s.id}
                  onEdit={() => {
                    setBanner(null);
                    pendingFocus.current = { row: s.id };
                    setDlg({ mode: 'edit', id: s.id });
                  }}
                  onTest={() => void runTest(s)}
                  onRemove={() => {
                    setBanner(null);
                    setConfirm(s);
                  }}
                />
              ))}
            </ul>
          )}
        </SettingsCard>
        <div className={styles.addBar}>
          <button
            type="button"
            ref={addRef}
            className={`${ui.btn} ${ui.primary}`}
            aria-disabled={full || !data.available || undefined}
            onClick={() => {
              if (full || !data.available) return;
              setBanner(null);
              pendingFocus.current = { row: null };
              setDlg({ mode: 'create' });
            }}
          >
            Add webhook
          </button>
          <span className={org.hint}>
            {full
              ? `You have ${limit} webhooks, the most you may have. Remove one to add another.`
              : `${data.items.length} of ${limit} webhooks.`}
          </span>
        </div>
      </SettingsSection>

      {dlg && (dlg.mode === 'create' || editing) && (
        <WebhookDialog
          key={dlg.mode === 'edit' ? dlg.id : 'create'}
          hook={editing}
          available={data.available}
          onClose={() => setDlg(null)}
        />
      )}
      {confirm && (
        <Dialog title={`Remove ${confirm.name}?`} width={460} onClose={() => setConfirm(null)}>
          <p>
            This deletes the stored webhook and the posts that are waiting for it.{' '}
            {kindOf(confirm.kind).label} itself is not changed: delete the webhook there too if you no longer
            want it.
          </p>
          <div className={styles.dlgActions}>
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost}`}
              onClick={() => setConfirm(null)}
              data-autofocus
            >
              Cancel
            </button>
            <button
              type="button"
              className={`${ui.btn} ${ui.danger}`}
              aria-disabled={remove.isPending || undefined}
              onClick={() => !remove.isPending && void doRemove(confirm)}
            >
              {remove.isPending ? 'Removing…' : 'Remove webhook'}
            </button>
          </div>
        </Dialog>
      )}
    </>
  );
}

function Row({
  s,
  result,
  testing,
  onEdit,
  onTest,
  onRemove,
}: {
  s: PersonalWebhook;
  result: { ok: boolean; text: string } | undefined;
  testing: boolean;
  onEdit: () => void;
  onTest: () => void;
  onRemove: () => void;
}) {
  const st = statusOf(s);
  const chip =
    st.tone === 'fail'
      ? `${styles.chip} ${styles.chipFail}`
      : st.tone === 'warn'
        ? `${styles.chip} ${styles.chipWarn}`
        : st.tone === 'off'
          ? `${styles.chip} ${styles.chipOff}`
          : styles.chip;
  return (
    <li className={styles.row} data-webhook={s.id}>
      <div className={styles.head}>
        <span className={styles.name}>{s.name}</span>
        <span className={styles.kind}>{kindOf(s.kind).label}</span>
        <span className={styles.host}>{s.host}</span>
        <span className={chip} data-testid="pwh-status">
          {st.text}
        </span>
      </div>
      <div className={styles.detail}>
        <p>{eventsText(s.events)}</p>
        {s.secretUnreadable && <p className={styles.problem}>{CHAT_WORDS.chat_unreadable}</p>}
        {s.lastError ? (
          <p className={styles.problem}>
            Last post failed. {CHAT_WORDS[s.lastError] ?? 'The service did not accept it.'}
          </p>
        ) : s.lastOkAt ? (
          <p>Last post worked on {when(s.lastOkAt)}.</p>
        ) : (
          <p>Nothing has been posted yet.</p>
        )}
      </div>
      <div className={styles.rowActions}>
        <button
          type="button"
          className={ui.btn}
          data-edit={s.id}
          aria-label={`Edit ${s.name}`}
          onClick={onEdit}
        >
          Edit
        </button>
        <button
          type="button"
          className={ui.btn}
          aria-label={`Send test message to ${s.name}`}
          aria-disabled={testing || s.secretUnreadable || undefined}
          onClick={() => {
            if (!testing && !s.secretUnreadable) onTest();
          }}
        >
          {testing ? 'Sending…' : 'Send test'}
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          aria-label={`Remove ${s.name}`}
          onClick={onRemove}
        >
          Remove
        </button>
      </div>
      <div role="status" aria-live="polite" className={styles.result} data-testid="pwh-test-result">
        {result && <p className={result.ok ? undefined : `${ui.banner} ${ui.errorBanner}`}>{result.text}</p>}
      </div>
    </li>
  );
}

/** Add (no `hook`) or change a webhook. The URL is typed here and never shown again. */
function WebhookDialog({
  hook,
  available,
  onClose,
}: {
  hook: PersonalWebhook | undefined;
  available: boolean;
  onClose: () => void;
}) {
  const create = useCreatePersonalWebhook();
  const update = useUpdatePersonalWebhook();
  const editing = hook !== undefined;
  const [kind, setKind] = useState<ChatKind>('slack');
  const [name, setName] = useState(hook?.name ?? '');
  const [url, setUrl] = useState('');
  const [picked, setPicked] = useState<string[]>(hook?.events ?? PERSONAL_EVENTS.map((e) => e.value));
  const [enabled, setEnabled] = useState(hook?.enabled ?? true);
  const [problem, setProblem] = useState<{ field: 'name' | 'url'; text: string } | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const nameRef = useRef<HTMLInputElement>(null);
  const urlRef = useRef<HTMLInputElement>(null);
  const flying = useRef(false);
  const k = kindOf(editing ? hook.kind : kind);
  const busy = create.isPending || update.isPending;

  function toggle(value: string, on: boolean) {
    setPicked((p) =>
      PERSONAL_EVENTS.map((e) => e.value as string).filter((v) => (v === value ? on : p.includes(v))),
    );
    setBanner(null);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (flying.current || busy || !available) return;
    setBanner(null);
    setProblem(null);
    const n = name.trim();
    if (n === '' || [...n].length > CHAT_MAX_NAME) {
      setProblem({ field: 'name', text: `Give the webhook a name of 1 to ${CHAT_MAX_NAME} characters.` });
      nameRef.current?.focus();
      return;
    }
    const u = url.trim();
    if (!editing && u === '') {
      setProblem({ field: 'url', text: 'Paste the webhook URL.' });
      urlRef.current?.focus();
      return;
    }
    if (u !== '' && !k.test(u)) {
      setProblem({ field: 'url', text: k.problem });
      urlRef.current?.focus();
      return;
    }
    flying.current = true;
    try {
      if (!editing) {
        await create.mutateAsync({
          name: n,
          kind,
          webhookUrl: u,
          events: picked as PersonalWebhookCreate['events'],
          ...(enabled ? {} : { enabled: false }),
        });
      } else {
        const patch: PersonalWebhookPatch = {};
        if (n !== hook.name) patch.name = n;
        if (enabled !== hook.enabled) patch.enabled = enabled;
        if (picked.join(',') !== hook.events.join(','))
          patch.events = picked as PersonalWebhookPatch['events'];
        if (u !== '') patch.webhookUrl = u;
        if (Object.keys(patch).length > 0) await update.mutateAsync({ id: hook.id, patch });
      }
      onClose();
    } catch (err) {
      setBanner(personalErrorText(err, editing ? 'save' : 'create', k.value));
    } finally {
      flying.current = false;
    }
  }

  return (
    <Dialog
      title={editing ? `Edit ${hook.name}` : 'Add a personal webhook'}
      width={560}
      onClose={onClose}
      className={styles.dlg}
    >
      <form
        onSubmit={(e) => void submit(e)}
        noValidate
        className={styles.fields}
        aria-label={editing ? 'Edit webhook' : 'Add webhook'}
      >
        {banner && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {banner}
          </p>
        )}
        {!editing && (
          <fieldset className={styles.kinds}>
            <legend>Service</legend>
            {CHAT_KINDS.map((c) => (
              <div key={c.value} className={styles.choice}>
                <input
                  id={`pwh-kind-${c.value}`}
                  data-autofocus={c.value === kind ? '' : undefined}
                  type="radio"
                  name="pwh-kind"
                  checked={kind === c.value}
                  onChange={() => {
                    setKind(c.value);
                    setProblem(null);
                  }}
                />
                <label htmlFor={`pwh-kind-${c.value}`}>{c.label}</label>
              </div>
            ))}
          </fieldset>
        )}
        <p className={styles.kindHint}>{k.hint}</p>
        <div className={ui.field}>
          <label htmlFor="pwh-name">Name</label>
          <input
            id="pwh-name"
            ref={nameRef}
            data-autofocus={editing ? '' : undefined}
            className={ui.input}
            type="text"
            maxLength={CHAT_MAX_NAME}
            autoComplete="off"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              setProblem(null);
            }}
            aria-invalid={problem?.field === 'name'}
            aria-describedby={problem?.field === 'name' ? 'pwh-name-problem' : undefined}
          />
          {problem?.field === 'name' && (
            <span id="pwh-name-problem" role="alert" className={org.fieldError}>
              {problem.text}
            </span>
          )}
        </div>
        <div className={ui.field}>
          <label htmlFor="pwh-url">{editing ? 'New webhook URL' : 'Webhook URL'}</label>
          <input
            id="pwh-url"
            ref={urlRef}
            className={`${ui.input} ${styles.secret}`}
            type="password"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            placeholder={k.placeholder}
            value={url}
            disabled={!available}
            onChange={(e) => {
              setUrl(e.target.value);
              setProblem(null);
            }}
            aria-invalid={problem?.field === 'url'}
            aria-describedby={problem?.field === 'url' ? 'pwh-url-problem' : 'pwh-url-hint'}
          />
          {problem?.field === 'url' ? (
            <span id="pwh-url-problem" role="alert" className={org.fieldError}>
              {problem.text}
            </span>
          ) : (
            <span id="pwh-url-hint" className={org.hint}>
              {editing
                ? `Saved (host ${hook.host}). Leave it empty to keep it, or type a new URL to replace it. The URL is a password: it is stored sealed and is never shown again.`
                : 'The URL is a password: it is stored sealed and is never shown again.'}
            </span>
          )}
        </div>
        {editing && (
          <div className={styles.choice}>
            <input
              id="pwh-enabled"
              type="checkbox"
              checked={enabled}
              onChange={(e) => {
                setEnabled(e.target.checked);
                setBanner(null);
              }}
              aria-describedby="pwh-enabled-hint"
            />
            <label htmlFor="pwh-enabled">
              <span>Post to this webhook</span>
              <span id="pwh-enabled-hint" className={styles.choiceHint}>
                Off drops the posts that are waiting. The webhook stays stored.
              </span>
            </label>
          </div>
        )}
        <fieldset className={styles.events}>
          <legend>Post when</legend>
          {PERSONAL_EVENTS.map((ev) => (
            <div key={ev.value} className={styles.choice}>
              <input
                id={`pwh-ev-${ev.value}`}
                type="checkbox"
                checked={picked.includes(ev.value)}
                onChange={(e) => toggle(ev.value, e.target.checked)}
                aria-describedby={`pwh-ev-${ev.value}-hint`}
              />
              <label htmlFor={`pwh-ev-${ev.value}`}>
                <span>{ev.label}</span>
                <span id={`pwh-ev-${ev.value}-hint`} className={styles.choiceHint}>
                  {ev.hint}
                </span>
              </label>
            </div>
          ))}
        </fieldset>
        <div className={styles.dlgActions}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className={`${ui.btn} ${ui.primary}`}
            aria-disabled={busy || !available || undefined}
          >
            {busy ? 'Saving' : editing ? 'Save' : 'Add webhook'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
