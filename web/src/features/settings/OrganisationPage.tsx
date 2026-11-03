import { useEffect, useRef, useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import { useOrgSlug } from '../../api/org';
import { useOrgDetail, useTeams, useUpdateOrg } from '../../api/queries';
import type { Org, Team } from '../../api/types';
import { Spinner } from '../../ui/States';
import { FixedError } from './FixedError';
import { OrgLogoField } from './OrgLogoField';
import { SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import { timeZoneList } from './timeZones';
import styles from './organisation.module.css';
import ui from '../../styles/ui.module.css';

/** The CSS class that holds the 40px touch targets (a test reads the CSS and this file). */
export const touchClass = styles.touch;

/** Zones grouped by the part before the first slash (Europe, America, ...); zones with no slash go under Other. */
function zoneGroups(zones: string[]): [string, string[]][] {
  const m = new Map<string, string[]>();
  for (const z of zones) {
    const r = z.includes('/') ? z.slice(0, z.indexOf('/')) : 'Other';
    m.set(r, [...(m.get(r) ?? []), z]);
  }
  return [...m.entries()];
}

const NAME_RULE = 'Enter 1 to 200 characters.';

/** A fixed sentence for a failed save. No server text reaches the screen. */
export function saveErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return 'The server did not accept this. Check the name, the time zone and the default team.';
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You need admin access to change the organisation.';
    case 404:
      return 'The server cannot find the organisation or the team. Reload the page and try again.';
    case 409:
      return 'Someone changed the organisation at the same time. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return 'Could not save the organisation. Try again.';
  }
}

/** Settings, Organisation: the name, the time zone and the default team (LAUN-98); the slug is fixed. */
export function OrganisationPage() {
  const slug = useOrgSlug();
  const org = useOrgDetail();
  const teams = useTeams();
  return (
    <SettingsPage title="Organisation">
      <div className={touchClass}>
        <SettingsSection title="General" description="The slug is fixed. You can change the rest.">
          {org.isPending ? (
            <Spinner label="Loading the organisation" />
          ) : org.isError ? (
            <FixedError
              title="Could not load the organisation"
              error={org.error}
              onRetry={() => void org.refetch()}
            />
          ) : (
            <OrgForm slug={slug} org={org.data} teams={teams.data ?? []} />
          )}
        </SettingsSection>
        {org.data && (
          <SettingsSection title="Logo" description="Shown in the sidebar and the organisation switcher.">
            <OrgLogoField org={org.data} />
          </SettingsSection>
        )}
      </div>
    </SettingsPage>
  );
}

type Field = 'name' | 'timeZone' | 'defaultTeam';

function OrgForm({ slug, org, teams }: { slug: string; org: Org; teams: readonly Team[] }) {
  const update = useUpdateOrg();
  const [name, setName] = useState(org.name);
  const [timeZone, setTimeZone] = useState(org.timeZone);
  const [defaultTeam, setDefaultTeam] = useState(org.defaultTeam ?? '');
  const [nameError, setNameError] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [focusField, setFocusField] = useState<Field | null>(null);
  const synced = useRef({ name: org.name, timeZone: org.timeZone, defaultTeam: org.defaultTeam ?? '' });

  // A change from outside (a second tab, another admin) fills the fields, unless the person is editing them.
  useEffect(() => {
    const last = synced.current;
    const next = { name: org.name, timeZone: org.timeZone, defaultTeam: org.defaultTeam ?? '' };
    if (next.name === last.name && next.timeZone === last.timeZone && next.defaultTeam === last.defaultTeam)
      return;
    if (name === last.name) setName(next.name);
    if (timeZone === last.timeZone) setTimeZone(next.timeZone);
    if (defaultTeam === last.defaultTeam) setDefaultTeam(next.defaultTeam);
    synced.current = next;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only a new server value may run this
  }, [org.name, org.timeZone, org.defaultTeam]);

  // Move focus after the error is on screen (state updates are batched, so the DOM is not ready in the handler).
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(`org-${focusField}`)?.focus();
    setFocusField(null);
  }, [focusField]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    // One call at a time. The button is aria-disabled, not disabled, so focus stays on it.
    if (update.isPending) return;
    setBanner(null);
    setSaved(false);
    const n = name.trim();
    if (n.length < 1 || [...n].length > 200) {
      setNameError(NAME_RULE);
      setFocusField('name');
      return;
    }
    setNameError(null);
    const body: { name?: string; timeZone?: string; defaultTeam?: string | null } = {};
    if (n !== org.name) body.name = n;
    if (timeZone !== org.timeZone) body.timeZone = timeZone;
    if (defaultTeam !== (org.defaultTeam ?? '')) body.defaultTeam = defaultTeam === '' ? null : defaultTeam;
    if (Object.keys(body).length === 0) {
      setBanner('Nothing changed.');
      return;
    }
    try {
      const out = await update.mutateAsync(body);
      synced.current = { name: out.name, timeZone: out.timeZone, defaultTeam: out.defaultTeam ?? '' };
      setName(out.name);
      setTimeZone(out.timeZone);
      setDefaultTeam(out.defaultTeam ?? '');
      setSaved(true);
    } catch (err) {
      setBanner(saveErrorText(err));
    }
  }

  const busy = update.isPending;
  return (
    <SettingsCard padded>
      <form onSubmit={(e) => void submit(e)} noValidate aria-label="Organisation settings">
        {banner && (
          <p role="alert" className={styles.problem}>
            {banner}
          </p>
        )}
        <div className={ui.field}>
          <label htmlFor="org-name">Name</label>
          <input
            id="org-name"
            className={ui.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            aria-invalid={!!nameError}
            aria-describedby={`org-name-hint${nameError ? ' org-name-error' : ''}`}
            autoComplete="off"
          />
          <span id="org-name-hint" className={styles.hint}>
            The name people see in the sidebar.
          </span>
          {nameError && (
            <span id="org-name-error" role="alert" className={styles.fieldError}>
              {nameError}
            </span>
          )}
        </div>
        <div className={ui.field}>
          <span className={ui.label}>Slug</span>
          <code>{slug}</code>
          <span className={styles.hint}>The organisation in every address. You cannot change it.</span>
        </div>
        <div className={ui.field}>
          <label htmlFor="org-timeZone">Time zone</label>
          <select
            id="org-timeZone"
            className={ui.select}
            value={timeZone}
            onChange={(e) => setTimeZone(e.target.value)}
            aria-describedby="org-timeZone-hint"
          >
            {zoneGroups(timeZoneList(org.timeZone)).map(([region, zones]) => (
              <optgroup key={region} label={region}>
                {zones.map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
          <span id="org-timeZone-hint" className={styles.hint}>
            The time zone of the organisation. A new organisation uses UTC. Launchpad does not use it for
            dates yet.
          </span>
        </div>
        <div className={ui.field}>
          <label htmlFor="org-defaultTeam">Default team</label>
          <select
            id="org-defaultTeam"
            className={ui.select}
            value={defaultTeam}
            onChange={(e) => setDefaultTeam(e.target.value)}
            aria-describedby="org-defaultTeam-hint"
          >
            <option value="">No default team</option>
            {teams.map((t) => (
              <option key={t.id} value={t.key}>
                {t.key} {t.name}
              </option>
            ))}
          </select>
          <span id="org-defaultTeam-hint" className={styles.hint}>
            The main team of the organisation. Launchpad does not use it for anything else yet.
          </span>
        </div>
        <div className={styles.actions}>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} aria-disabled={busy || undefined}>
            {busy ? 'Saving' : 'Save'}
          </button>
          <p role="status" className={styles.ok}>
            {saved ? 'Saved.' : ''}
          </p>
        </div>
      </form>
    </SettingsCard>
  );
}
