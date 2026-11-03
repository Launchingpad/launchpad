import { useEffect, useRef, useState, type FormEvent } from 'react';
import {
  useChatServices,
  useCreateChatService,
  useRemoveChatService,
  useTeams,
  useTestChatService,
  useUpdateChatService,
} from '../../api/queries';
import type { ChatService, ChatServiceCreate, ChatServiceList, ChatServicePatch } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { Spinner } from '../../ui/States';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import {
  CHAT_EVENTS,
  CHAT_KINDS,
  CHAT_LIMIT_FALLBACK,
  CHAT_MAX_NAME,
  CHAT_WORDS,
  chatErrorText,
  kindOf,
  type ChatKind,
} from './chatText';
import org from './organisation.module.css';
import styles from './chatServices.module.css';
import ui from '../../styles/ui.module.css';

function when(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

/**
 * Settings, Chat services (LAUN-204, decisions-T36; teams LAUN-209, decisions-T38): up to ten Slack, Discord,
 * Fluxer or custom webhooks for the org, each in one team, with its own name, event kinds and on/off switch. A webhook URL is a secret and is write-only: the page
 * shows the host name, never the URL.
 */
export function ChatServicesPage() {
  const list = useChatServices();
  return (
    <SettingsPage
      title="Chat services"
      description="Post chosen events to Slack, Discord, Fluxer or any service that takes a webhook. A post says what happened and links to Launchpad. It never holds a title, a comment or a person's name, because the whole channel can read it. A state change names the old and the new state. Each service belongs to one team and posts only the events of that team's issues."
    >
      <div className={org.touch}>
        {list.isPending ? (
          <Spinner label="Loading the chat services" />
        ) : list.isError ? (
          <FixedError
            title="Could not load the chat services"
            error={list.error}
            onRetry={() => void list.refetch()}
          />
        ) : (
          <ChatBody data={list.data} />
        )}
      </div>
    </SettingsPage>
  );
}

type Dlg = { mode: 'create' } | { mode: 'edit'; id: string } | null;

function statusOf(s: ChatService): { text: string; tone: 'ok' | 'fail' | 'warn' | 'off' | 'plain' } {
  if (!s.enabled) return { text: 'Off', tone: 'off' };
  if (s.secretUnreadable) return { text: 'Webhook unreadable', tone: 'warn' };
  if (s.lastError) return { text: 'Last post failed', tone: 'fail' };
  if (s.lastOkAt) return { text: 'Working', tone: 'ok' };
  return { text: 'Not used yet', tone: 'plain' };
}

function ChatBody({ data }: { data: ChatServiceList }) {
  const remove = useRemoveChatService();
  const test = useTestChatService();
  const teams = useTeams();
  const [dlg, setDlg] = useState<Dlg>(null);
  const [confirm, setConfirm] = useState<ChatService | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [results, setResults] = useState<Record<string, { ok: boolean; text: string }>>({});
  const [testing, setTesting] = useState<string | null>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const addRef = useRef<HTMLButtonElement>(null);
  // Synchronous in-flight guards: two clicks in one tick would both pass an isPending check.
  const flying = useRef({ test: false, remove: false });
  // Where focus goes once a dialog has closed. The Dialog puts focus back on its opener; this covers an
  // opener that is gone (a removed row) or never took focus (Safari does not focus a button on click).
  const pendingFocus = useRef<{ row: string | null; removed?: string } | null>(null);
  const limit = data.limit || CHAT_LIMIT_FALLBACK;
  const full = data.items.length >= limit;

  useEffect(() => {
    const p = pendingFocus.current;
    // The dialog's own cleanup restores focus first; this effect runs after it.
    if (dlg || confirm || !p) return;
    // A removal waits until the row is gone: the dialog may have put focus back on its Remove button.
    if (p.removed && data.items.some((x) => x.id === p.removed)) return;
    pendingFocus.current = null;
    const a = document.activeElement;
    if (a && a !== document.body && a.isConnected) return;
    const row = (id: string | null) =>
      id ? listRef.current?.querySelector<HTMLElement>(`[data-edit="${id}"]`) : null;
    const target = row(p.row) ?? addRef.current ?? document.querySelector<HTMLElement>('h1');
    target?.focus();
  }, [dlg, confirm, data.items]);

  // Another session removed the service this dialog is about: the dialog has gone with the row, so its
  // opener is gone too. Arm the focus fallback and close the state that is left.
  const editGone = dlg?.mode === 'edit' && !data.items.some((x) => x.id === dlg.id);
  const confirmGone = !!confirm && !flying.current.remove && !data.items.some((x) => x.id === confirm.id);
  useEffect(() => {
    if (editGone) {
      pendingFocus.current = { row: null };
      setDlg(null);
    }
    if (confirmGone) {
      pendingFocus.current = { row: null };
      setConfirm(null);
    }
  }, [editGone, confirmGone]);

  async function runTest(s: ChatService) {
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
      setResults((r) => ({ ...r, [s.id]: { ok: false, text: chatErrorText(err, 'test', s.kind) } }));
    } finally {
      flying.current.test = false;
      setTesting(null);
    }
  }

  async function doRemove(s: ChatService) {
    if (flying.current.remove) return;
    flying.current.remove = true;
    const rows = data.items;
    const i = rows.findIndex((r) => r.id === s.id);
    pendingFocus.current = { row: (rows[i + 1] ?? rows[i - 1])?.id ?? null, removed: s.id };
    setBanner(null);
    try {
      await remove.mutateAsync(s.id);
    } catch (err) {
      // A service that is already gone (404 does not occur: DELETE is idempotent) is the state wanted.
      setBanner(chatErrorText(err, 'remove', s.kind));
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
        title="Services"
        description="Each service gets one post for each event of the kinds you tick, for the issues of its own team, whatever each person has chosen for their own email and inbox. A post goes out whoever the issue belongs to and whoever made the change, a person or an agent. After an upgrade, an existing service posts only the events of the team it was moved to; add a service per team to get the others. A team with no service gets no post. An event that belongs to no team (a project with no team, for example) posts nowhere. If any active member has muted an issue, that issue posts nothing. Launchpad cannot see who is in a channel: everyone in it sees the posts, whether or not they have access to the team."
      >
        <SettingsCard>
          {data.items.length === 0 ? (
            <p className={styles.empty}>No chat service yet. Add one to start posting.</p>
          ) : (
            <ul className={styles.list} ref={listRef} aria-label="Chat services">
              {data.items.map((s) => (
                <ServiceRow
                  key={s.id}
                  s={s}
                  teamName={teams.data?.find((t) => t.id === s.teamId)?.name}
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
            Add service
          </button>
          <span className={org.hint}>
            {full
              ? `You have ${limit} services, the most an org may have. Remove one to add another.`
              : `${data.items.length} of ${limit} services.`}
          </span>
        </div>
      </SettingsSection>

      {dlg && (dlg.mode === 'create' || editing) && (
        <ServiceDialog
          key={dlg.mode === 'edit' ? dlg.id : 'create'}
          service={editing}
          teams={teams.data ?? []}
          available={data.available}
          onClose={() => setDlg(null)}
        />
      )}
      {confirm && (
        <RemoveDialog
          service={confirm}
          pending={remove.isPending}
          onClose={() => setConfirm(null)}
          onConfirm={() => void doRemove(confirm)}
        />
      )}
    </>
  );
}

function ServiceRow({
  s,
  teamName,
  result,
  testing,
  onEdit,
  onTest,
  onRemove,
}: {
  s: ChatService;
  teamName: string | undefined;
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
  const kinds = s.events.length;
  return (
    <li className={styles.row} data-service={s.id}>
      <div className={styles.head}>
        <span className={styles.name}>{s.name}</span>
        <span className={styles.kind}>{kindOf(s.kind).label}</span>
        <span className={styles.host}>{s.host}</span>
        <span className={styles.kind} data-testid="chat-team">
          Team {teamName ?? '…'}
        </span>
        <span className={chip} data-testid="chat-status">
          {st.text}
        </span>
      </div>
      <div className={styles.detail}>
        <p>
          {kinds === 0
            ? 'No event kinds are ticked, so nothing is posted.'
            : `Posts ${kinds} ${kinds === 1 ? 'kind' : 'kinds'} of event.`}
        </p>
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
        {s.rateCappedCount > 0 && (
          <p className={styles.problem} data-testid="chat-capped">
            The hourly limit dropped {s.rateCappedCount} {s.rateCappedCount === 1 ? 'post' : 'posts'} in the
            last day{s.lastRateCappedAt ? `, the latest on ${when(s.lastRateCappedAt)}` : ''}.
          </p>
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
      <div role="status" aria-live="polite" className={styles.result} data-testid="chat-test-result">
        {result && <p className={result.ok ? undefined : `${ui.banner} ${ui.errorBanner}`}>{result.text}</p>}
      </div>
    </li>
  );
}

/** Add (no `service`) or change a service. The URL is typed here and never shown again. */
function ServiceDialog({
  service,
  teams,
  available,
  onClose,
}: {
  service: ChatService | undefined;
  teams: { id: string; key: string; name: string }[];
  available: boolean;
  onClose: () => void;
}) {
  const create = useCreateChatService();
  const update = useUpdateChatService();
  const editing = service !== undefined;
  const [kind, setKind] = useState<ChatKind>('slack');
  const [name, setName] = useState(service?.name ?? '');
  const [url, setUrl] = useState('');
  // The team is required. A new service starts on the first team; the admin can change it.
  const [pickedTeam, setPickedTeam] = useState(service?.teamId ?? '');
  const teamId = pickedTeam !== '' ? pickedTeam : (teams[0]?.id ?? '');
  const [picked, setPicked] = useState<string[]>(service?.events ?? []);
  const [enabled, setEnabled] = useState(service?.enabled ?? true);
  const [problem, setProblem] = useState<{ field: 'name' | 'url'; text: string } | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const nameRef = useRef<HTMLInputElement>(null);
  const urlRef = useRef<HTMLInputElement>(null);
  const flying = useRef(false);
  const k = kindOf(editing ? service.kind : kind);
  // LAUN-231 (decisions-T20, "Not done" chat item): team access does not apply to a channel.
  const teamKey = teams.find((t) => t.id === teamId)?.key;
  const teamWords = teamKey ? `team ${teamKey}` : 'this team';
  const busy = create.isPending || update.isPending;

  function toggle(value: string, on: boolean) {
    setPicked((p) =>
      CHAT_EVENTS.map((e) => e.value as string).filter((v) => (v === value ? on : p.includes(v))),
    );
    setBanner(null);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (flying.current || busy || !available) return;
    setBanner(null);
    setProblem(null);
    const n = name.trim();
    if (n === '' || [...n].length > 40) {
      setProblem({ field: 'name', text: 'Give the service a name of 1 to 40 characters.' });
      nameRef.current?.focus();
      return;
    }
    if (teamId === '') {
      setBanner('Choose the team this service posts for.');
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
          teamId,
          webhookUrl: u,
          events: picked as ChatServiceCreate['events'],
          ...(enabled ? {} : { enabled: false }),
        });
      } else {
        const patch: ChatServicePatch = {};
        if (n !== service.name) patch.name = n;
        if (teamId !== service.teamId) patch.teamId = teamId;
        if (enabled !== service.enabled) patch.enabled = enabled;
        if (picked.join(',') !== service.events.join(','))
          patch.events = picked as ChatServicePatch['events'];
        if (u !== '') patch.webhookUrl = u;
        if (Object.keys(patch).length > 0) await update.mutateAsync({ id: service.id, patch });
      }
      onClose();
    } catch (err) {
      setBanner(chatErrorText(err, editing ? 'save' : 'create', k.value));
    } finally {
      flying.current = false;
    }
  }

  return (
    <Dialog
      title={editing ? `Edit ${service.name}` : 'Add a chat service'}
      width={560}
      onClose={onClose}
      className={styles.dlg}
    >
      <form
        onSubmit={(e) => void submit(e)}
        noValidate
        className={styles.fields}
        aria-label={editing ? 'Edit service' : 'Add service'}
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
                  id={`chat-kind-${c.value}`}
                  data-autofocus={c.value === kind ? '' : undefined}
                  type="radio"
                  name="chat-kind"
                  checked={kind === c.value}
                  onChange={() => {
                    setKind(c.value);
                    setProblem(null);
                  }}
                />
                <label htmlFor={`chat-kind-${c.value}`}>{c.label}</label>
              </div>
            ))}
          </fieldset>
        )}
        <p className={styles.kindHint}>{k.hint}</p>
        <div className={ui.field}>
          <label htmlFor="chat-name">Name</label>
          <input
            id="chat-name"
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
            aria-describedby={problem?.field === 'name' ? 'chat-name-problem' : undefined}
          />
          {problem?.field === 'name' && (
            <span id="chat-name-problem" role="alert" className={org.fieldError}>
              {problem.text}
            </span>
          )}
        </div>
        <div className={ui.field}>
          <label htmlFor="chat-team">Team</label>
          <select
            id="chat-team"
            className={ui.input}
            value={teamId}
            required
            onChange={(e) => {
              setPickedTeam(e.target.value);
              setBanner(null);
            }}
            aria-describedby="chat-team-hint chat-team-warning"
          >
            {teamId === '' && <option value="">Choose a team</option>}
            {teams.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name} ({t.key})
              </option>
            ))}
          </select>
          <span id="chat-team-hint" className={org.hint}>
            The service posts only the events of this team's issues. You can move it to another team later.
          </span>
          <p id="chat-team-warning" role="note" className={styles.warn} data-testid="chat-team-warning">
            Everyone in this channel sees each post this service sends for {teamWords}: what happened, a link,
            the issue identifier, and for a state change the names of the old and new state. They see it
            whether or not they have access to {teamWords}. Launchpad cannot see who is in the channel.
          </p>
        </div>
        <div className={ui.field}>
          <label htmlFor="chat-url">{editing ? 'New webhook URL' : 'Webhook URL'}</label>
          <input
            id="chat-url"
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
            aria-describedby={problem?.field === 'url' ? 'chat-url-problem' : 'chat-url-hint'}
          />
          {problem?.field === 'url' ? (
            <span id="chat-url-problem" role="alert" className={org.fieldError}>
              {problem.text}
            </span>
          ) : (
            <span id="chat-url-hint" className={org.hint}>
              {editing
                ? `Leave it empty to keep the stored URL (host ${service.host}). The URL is a password: it is stored sealed and is never shown again.${service.kind === 'custom' || service.kind === 'fluxer' ? ` A ${service.kind === 'fluxer' ? 'Fluxer' : 'custom'} service that moves to another host needs the whole new URL.` : ''}`
                : 'The URL is a password: it is stored sealed and is never shown again.'}
            </span>
          )}
        </div>
        {editing && (
          <div className={styles.choice}>
            <input
              id="chat-enabled"
              type="checkbox"
              checked={enabled}
              onChange={(e) => {
                setEnabled(e.target.checked);
                setBanner(null);
              }}
              aria-describedby="chat-enabled-hint"
            />
            <label htmlFor="chat-enabled">
              <span>Post events to this service</span>
              <span id="chat-enabled-hint" className={styles.choiceHint}>
                Off drops the posts that are waiting. The webhook stays stored.
              </span>
            </label>
          </div>
        )}
        <fieldset className={styles.events}>
          <legend>Post these events</legend>
          {CHAT_EVENTS.map((ev) => (
            <div key={ev.value} className={styles.choice}>
              <input
                id={`chat-ev-${ev.value}`}
                type="checkbox"
                checked={picked.includes(ev.value)}
                onChange={(e) => toggle(ev.value, e.target.checked)}
                aria-describedby={`chat-ev-${ev.value}-hint`}
              />
              <label htmlFor={`chat-ev-${ev.value}`}>
                <span>{ev.label}</span>
                <span id={`chat-ev-${ev.value}-hint`} className={styles.choiceHint}>
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
            {busy ? 'Saving' : editing ? 'Save' : 'Add service'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function RemoveDialog({
  service,
  pending,
  onClose,
  onConfirm,
}: {
  service: ChatService;
  pending: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Dialog title={`Remove ${service.name}?`} width={460} onClose={onClose}>
      <p>
        This deletes the stored webhook, the event list and the posts that are waiting for this service.{' '}
        {kindOf(service.kind).label} itself is not changed: delete the webhook there too if you no longer want
        it.
      </p>
      <div className={styles.dlgActions}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          aria-disabled={pending || undefined}
          onClick={() => !pending && onConfirm()}
        >
          {pending ? 'Removing…' : 'Remove service'}
        </button>
      </div>
    </Dialog>
  );
}
