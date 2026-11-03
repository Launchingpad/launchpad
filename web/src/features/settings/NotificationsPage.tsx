import { useId, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { ApiError } from '../../api/errors';
import { useOrgSlug } from '../../api/org';
import { useNotificationSettings, useNotificationWrites, useTeams } from '../../api/queries';
import type {
  NotificationEvent,
  NotificationEventInput,
  NotificationEventSettings,
  NotificationSettings,
  NotificationSource,
  NotificationValue,
  Team,
} from '../../api/types';
import { Spinner } from '../../ui/States';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage, SettingsRow, SettingsSection, SettingsToggle } from './SettingsParts';
import styles from './notifications.module.css';

// LAUN-36. Settings > Notifications, for every member (decisions-T13 #38). The server computes every resolved
// value and its source; this page only shows them. Each change saves at once and the reply replaces the view.
// No text from the server reaches the screen: every message below is fixed.

type Channel = 'email' | 'inbox' | 'push';

const EVENTS: Record<NotificationEvent, { label: string; text: string }> = {
  assigned: { label: 'Assigned to me', text: 'An issue is assigned to you.' },
  mention: { label: 'Mentions', text: 'Someone mentions you with @name.' },
  comment: { label: 'Comments', text: 'A new comment on an issue you made, own or took part in.' },
  state_change: { label: 'Status changes', text: 'The state of one of those issues changes.' },
  new_issue: { label: 'New issues', text: 'A new issue in a team you follow. Follow the team below first.' },
  project_update_due: {
    label: 'Project updates due',
    text: 'A project you lead is due for an update. Applies to the whole organisation.',
  },
};

const SOURCE: Record<NotificationSource, string> = {
  team: 'set for this team',
  global: 'your setting',
  default: 'the default',
};

const word = (v: NotificationValue) => (v === 'on' ? 'On' : 'Off');

/** The fixed text for a failed save. */
export function saveText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not save. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You cannot change these settings.';
    case 404:
      return 'This team no longer exists. Reload the page.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return 'Could not save. Try again.';
  }
}

/**
 * One three-state control as radio buttons (LAUN-136): the unset choice names the value it would give, then On and
 * Off. Native radios give the arrow keys; the group is named "<event>, <channel>" and described by its Now line.
 */
function ValueRadios({
  id,
  label,
  stored,
  inheritLabel,
  describedBy,
  onChange,
}: {
  describedBy: string;
  id: string;
  label: string;
  stored: NotificationValue | null;
  inheritLabel: string;
  onChange: (next: NotificationValue | null) => void;
}) {
  const choices: { key: string; value: NotificationValue | null; text: string }[] = [
    { key: 'unset', value: null, text: inheritLabel },
    { key: 'on', value: 'on', text: 'On' },
    { key: 'off', value: 'off', text: 'Off' },
  ];
  return (
    <div role="radiogroup" aria-label={label} aria-describedby={describedBy} className={styles.radios}>
      {choices.map((c) => (
        <label key={c.key} className={styles.radio}>
          <input type="radio" name={id} checked={stored === c.value} onChange={() => onChange(c.value)} />
          <span>{c.text}</span>
        </label>
      ))}
    </div>
  );
}

function Resolved({
  id,
  channel,
  value,
  source,
  masterOff,
  followOnly,
}: {
  id: string;
  followOnly: boolean;
  channel: Channel;
  value: NotificationValue;
  source: NotificationSource;
  masterOff: boolean;
}) {
  const text =
    channel === 'email' && masterOff
      ? 'Off: email is turned off above'
      : `Now ${word(value)} (${SOURCE[source]})`;
  return (
    <>
      <span id={id} className={styles.resolved}>
        {text}
      </span>
      {followOnly && (
        <span id={`${id}-follow`} className={styles.resolved}>
          Only for teams you follow
        </span>
      )}
    </>
  );
}

type Save = (event: NotificationEvent, patch: NotificationEventInput) => void;

function EventRow({
  ev,
  scope,
  settings,
  teamId,
  save,
}: {
  ev: NotificationEventSettings;
  scope: 'global' | 'team';
  settings: NotificationSettings;
  teamId?: string;
  save: Save;
}) {
  const uid = useId();
  const info = EVENTS[ev.event];
  const team = scope === 'team' ? ev.teams.find((t) => t.teamId === teamId) : undefined;
  return (
    <div className={styles.event} role="group" aria-label={info.label}>
      <div className={styles.eventText}>
        <span className={styles.eventName}>{info.label}</span>
        <span className={styles.eventHint}>{info.text}</span>
      </div>
      {(settings.push.available && settings.push.hasDevice
        ? (['email', 'inbox', 'push'] as const)
        : (['email', 'inbox'] as const)
      ).map((ch) => {
        const stored = scope === 'team' ? (team?.override[ch] ?? null) : ev.global[ch];
        const resolved = scope === 'team' ? (team?.resolved[ch] ?? ev.resolved[ch]) : ev.resolved[ch];
        const inherited = scope === 'team' ? (ev.global[ch] ?? ev.default[ch]) : ev.default[ch];
        const id = `${uid}-${ch}`;
        const name = ch === 'email' ? 'Email' : ch === 'inbox' ? 'Inbox' : 'Push';
        return (
          <div key={ch} className={styles.channel}>
            <span className={styles.channelLabel} aria-hidden="true">
              {name}
            </span>
            <ValueRadios
              id={id}
              label={`${info.label}, ${name.toLowerCase()}`}
              stored={stored}
              inheritLabel={`${scope === 'team' ? 'Inherit' : 'Default'} (${word(inherited)})`}
              describedBy={ev.requiresFollow ? `${id}-now ${id}-now-follow` : `${id}-now`}
              onChange={(next) => save(ev.event, { [ch]: next })}
            />
            <Resolved
              id={`${id}-now`}
              followOnly={ev.requiresFollow}
              channel={ch}
              value={resolved.value}
              source={resolved.source}
              masterOff={!settings.emailEnabled}
            />
          </div>
        );
      })}
    </div>
  );
}

function TeamBlock({
  team,
  settings,
  following,
  onFollow,
  save,
}: {
  team: Team;
  settings: NotificationSettings;
  following: boolean;
  onFollow: (on: boolean) => void;
  save: (event: NotificationEvent, patch: NotificationEventInput) => void;
}) {
  const [open, setOpen] = useState(false);
  const panel = useId();
  return (
    <div className={styles.team}>
      <SettingsRow
        label={`${team.name} (${team.key})`}
        description="Follow the team to hear about its new issues."
      >
        <SettingsToggle checked={following} onChange={onFollow} label={`Follow ${team.name}`} />
      </SettingsRow>
      <div className={styles.overrideBar}>
        <button
          type="button"
          className={styles.disclosure}
          aria-expanded={open}
          aria-controls={panel}
          onClick={() => setOpen(!open)}
        >
          <span className={styles.glyph} aria-hidden="true" />
          Override for {team.key}
        </button>
      </div>
      {open && (
        <div id={panel} className={styles.panel} role="region" aria-label={`Overrides for ${team.name}`}>
          {settings.events
            .filter((ev) => ev.teams.length > 0) /* project_update_due has no team level */
            .map((ev) => (
              <EventRow
                key={ev.event}
                ev={ev}
                scope="team"
                teamId={team.id}
                settings={settings}
                save={save}
              />
            ))}
        </div>
      )}
    </div>
  );
}

export function NotificationsPage() {
  const org = useOrgSlug();
  const q = useNotificationSettings();
  const teams = useTeams();
  const writes = useNotificationWrites();
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  // Writes go one after the other, so a slow reply never lands over a newer one.
  const chain = useRef<Promise<void>>(Promise.resolve());

  function run(job: () => Promise<void>) {
    chain.current = chain.current.then(async () => {
      try {
        await job();
        setError(null);
        setSaved(true);
      } catch (e) {
        setSaved(false);
        setError(saveText(e));
      }
    });
  }

  if (q.isLoading || teams.isLoading) {
    return (
      <SettingsPage title="Notifications">
        <Spinner />
      </SettingsPage>
    );
  }
  if (q.isError || !q.data) {
    return (
      <SettingsPage title="Notifications">
        <FixedError
          error={q.error}
          onRetry={() => void q.refetch()}
          title="Could not load your notification settings"
        />
      </SettingsPage>
    );
  }
  const s = q.data;
  const saveGlobalEvent: Save = (event, patch) =>
    run(() => writes.saveGlobal({ events: { [event]: patch } }));

  return (
    <SettingsPage
      title="Notifications"
      description="Choose what Launchpad tells you about, and where. These settings are yours only."
      wide
    >
      <div role="status" className={styles.saved}>
        {saved && !error ? 'Saved.' : ''}
      </div>
      {error && (
        <p role="alert" className={styles.error}>
          {error}
        </p>
      )}
      {s.push.available && s.push.hasDevice && (
        <p role="note" className={styles.note}>
          Push wakes the Launchpad mobile app on the devices you registered. It is off until you turn it on
          for an event. A wake holds no text of the issue.
        </p>
      )}
      {s.push.available && !s.push.hasDevice && (
        <p role="note" className={styles.note}>
          Your administrator has turned on mobile push, but you have no registered device, so there is no Push
          choice yet. Sign in to the mobile app, then see{' '}
          <Link to={`/${org}/settings/mobile-devices`}>Mobile devices</Link>.
        </p>
      )}
      {!s.mail.available && (
        <p role="note" className={styles.note}>
          This install cannot send email, so nothing below reaches your mailbox. The inbox still works.
        </p>
      )}

      <SettingsSection title="Email">
        <SettingsCard>
          <SettingsRow
            label="Email notifications"
            description="The master switch. Off stops all email, whatever you choose below."
          >
            <SettingsToggle
              checked={s.emailEnabled}
              onChange={(on) => run(() => writes.saveGlobal({ emailEnabled: on }))}
              label="Email notifications"
            />
          </SettingsRow>
          <SettingsRow
            label="Include a short excerpt"
            description="Add the first 200 characters of a comment to the email. Off keeps the text out of your mailbox."
          >
            <SettingsToggle
              checked={s.includeExcerpt}
              onChange={(on) => run(() => writes.saveGlobal({ includeExcerpt: on }))}
              label="Include a short excerpt"
            />
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>

      <SettingsSection
        title="What you hear about"
        description="Each choice is Default, On or Off. The line under a choice shows the value that applies now and where it comes from."
      >
        <SettingsCard>
          {s.events.map((ev) => (
            <EventRow key={ev.event} ev={ev} scope="global" settings={s} save={saveGlobalEvent} />
          ))}
        </SettingsCard>
      </SettingsSection>

      <SettingsSection
        title="Teams"
        description="Follow a team to hear about its new issues. Override a choice for one team when it needs its own rule; Inherit takes the value from your settings above."
      >
        {teams.isError ? (
          <FixedError
            error={teams.error}
            onRetry={() => void teams.refetch()}
            title="Could not load your teams"
          />
        ) : (teams.data ?? []).length === 0 ? (
          <p className={styles.note}>There are no teams yet.</p>
        ) : (
          <SettingsCard>
            {(teams.data ?? []).map((t) => (
              <TeamBlock
                key={t.id}
                team={t}
                settings={s}
                following={s.follows.includes(t.id)}
                onFollow={(on) => run(() => writes.follow(t, on))}
                save={(event, patch) => run(() => writes.saveTeam(t.key, { events: { [event]: patch } }))}
              />
            ))}
          </SettingsCard>
        )}
      </SettingsSection>
    </SettingsPage>
  );
}
