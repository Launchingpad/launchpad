import { productTeamsTitle, teamKeysShort } from '../../ui/teamKeys';
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { useMatch, useNavigate } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useIssueSearch, useProducts, useTeams } from '../../api/queries';
import { useAuthedSession, useLogout } from '../auth/session';
import { useUi } from '../shell/ui-context';
import { LOOKS, useTheme } from '../../lib/theme';
import { useChooseTheme } from '../settings/useChooseTheme';
import { THEME_NAME } from '../settings/appearanceText';
import { Dialog } from '../../ui/Dialog';
import { StatusIcon } from '../../ui/Icons';
import ui from '../../styles/ui.module.css';
import styles from './palette.module.css';

interface Item {
  id: string;
  label: string;
  hint?: string;
  title?: string;
  group: string;
  icon?: React.ReactNode;
  run: () => void;
}

const IDENT = /^[a-z][a-z0-9]*-\d+$/i;
/** A bare ticket number (`113`) or an identifier (`ORB-113`). */
const TICKET = /^(?:[a-z][a-z0-9]*-)?\d+$/i;

/**
 * LAUN-114: the team the palette was opened from: a team page (`/team/ORB/...`), or the team of the
 * issue page (the identifier's prefix). Undefined on All issues, My issues and the rest.
 */
function useTeamContext(teamKeys: string[] | undefined): string | undefined {
  const onTeam = useMatch('/:org/team/:team/*');
  const onIssue = useMatch('/:org/issue/:issue');
  const keys = teamKeys ?? [];
  const raw = onTeam?.params.team ?? onIssue?.params.issue?.slice(0, onIssue.params.issue.lastIndexOf('-'));
  return raw ? keys.find((k) => k.toLowerCase() === raw.toLowerCase()) : undefined;
}

export function CommandPalette() {
  const { paletteOpen } = useUi();
  return paletteOpen ? <Palette /> : null;
}

function Palette() {
  const org = useOrgSlug();
  const nav = useNavigate();
  const { orgs } = useAuthedSession();
  const logout = useLogout();
  const teams = useTeams();
  const products = useProducts();
  const { setPaletteOpen, openCreate, setHelpOpen } = useUi();
  const { setPref } = useTheme();
  const chooseTheme = useChooseTheme();
  const [q, setQ] = useState('');
  const [debounced, setDebounced] = useState('');
  const [active, setActive] = useState(0);
  const listId = useId();
  useEffect(() => {
    const t = setTimeout(() => setDebounced(q), 180);
    return () => clearTimeout(t);
  }, [q]);
  const teamKey = useTeamContext(teams.data?.map((t) => t.key));
  // Per palette opening only: closing the palette forgets the choice.
  const [allTeams, setAllTeams] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  // A ticket number finds the ticket in ANY team, so the scope does not apply to it.
  const ticketQuery = TICKET.test(debounced.trim());
  const scoped = teamKey !== undefined && !allTeams && !ticketQuery;
  const found = useIssueSearch(debounced, true, scoped ? teamKey : '');
  const close = () => setPaletteOpen(false);
  const go = (path: string) => () => {
    close();
    nav(path);
  };

  const items = useMemo<Item[]>(() => {
    const text = q.trim().toLowerCase();
    const actions: Item[] = [
      {
        id: 'create',
        group: 'Actions',
        label: 'Create issue',
        hint: 'C',
        run: () => {
          close();
          openCreate();
        },
      },
      { id: 'mine', group: 'Go to', label: 'My issues', run: go(`/${org}/my-issues`) },
      { id: 'all', group: 'Go to', label: 'All issues', run: go(`/${org}/issues`) },
      { id: 'projects', group: 'Go to', label: 'Projects', run: go(`/${org}/projects`) },
      ...(teams.data ?? []).map((t) => ({
        id: `team-${t.id}`,
        group: 'Switch team',
        label: `${t.name} (${t.key})`,
        run: go(`/${org}/team/${t.key}`),
      })),
      ...(products.data ?? []).map((p) => ({
        id: `product-${p.id}`,
        group: 'Go to product',
        label: `${p.name} (${teamKeysShort(p.teamKeys)})`,
        title: productTeamsTitle(p.name, p.teamKeys),
        run: go(`/${org}/product/${p.slug}`),
      })),
      ...orgs
        .filter((o) => o.slug !== org)
        .map((o) => ({
          id: `org-${o.id}`,
          group: 'Switch organisation',
          label: o.name,
          run: go(`/${o.slug}/issues`),
        })),
      // LAUN-235: the theme (Linear, Paper, Terminal), saved for the person like the Appearance page does.
      ...LOOKS.map((l) => ({
        id: `look-${l}`,
        group: 'Theme',
        label: `Theme: ${THEME_NAME[l]}`,
        run: () => {
          chooseTheme(l);
          close();
        },
      })),
      {
        id: 'theme-light',
        group: 'Light or dark',
        label: 'Light mode',
        run: () => {
          setPref('light');
          close();
        },
      },
      {
        id: 'theme-dark',
        group: 'Light or dark',
        label: 'Dark mode',
        run: () => {
          setPref('dark');
          close();
        },
      },
      {
        id: 'theme-system',
        group: 'Light or dark',
        label: 'Match system mode',
        run: () => {
          setPref('system');
          close();
        },
      },
      {
        id: 'help',
        group: 'Help',
        label: 'Keyboard shortcuts',
        hint: '?',
        run: () => {
          close();
          setHelpOpen(true);
        },
      },
      {
        id: 'logout',
        group: 'Account',
        label: 'Sign out',
        run: () => {
          close();
          void logout();
        },
      },
    ];
    const filtered = text
      ? actions.filter((a) => `${a.label} ${a.group}`.toLowerCase().includes(text))
      : actions;
    const out: Item[] = [];
    if (IDENT.test(q.trim())) {
      const id = q.trim().toUpperCase();
      out.push({ id: `open-${id}`, group: 'Issues', label: `Open ${id}`, run: go(`/${org}/issue/${id}`) });
    }
    for (const i of text ? (found.data ?? []) : []) {
      if (out.some((o) => o.id === `open-${i.identifier}`)) continue;
      out.push({
        id: `issue-${i.id}`,
        group: 'Issues',
        label: `${i.identifier}  ${i.title}`,
        icon: <StatusIcon type={i.state.type} color={i.state.color} name={i.state.name} />,
        run: go(`/${org}/issue/${i.identifier}`),
      });
    }
    return [...out, ...filtered];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, found.data, teams.data, products.data, orgs, org]);

  const idx = Math.min(active, Math.max(0, items.length - 1));
  useEffect(() => {
    document.getElementById(`${listId}-${idx}`)?.scrollIntoView?.({ block: 'nearest' });
  }, [idx, listId]);

  let lastGroup = '';
  return (
    <Dialog title="Command palette" onClose={close} hideHead width={560}>
      <div className={styles.wrap}>
        <input
          ref={inputRef}
          data-autofocus
          className={styles.input}
          role="combobox"
          aria-expanded="true"
          aria-controls={listId}
          aria-activedescendant={items.length ? `${listId}-${idx}` : undefined}
          aria-label="Search issues and commands"
          placeholder="Search issues, or type a command…"
          autoComplete="off"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setActive(0);
          }}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault();
              setActive((idx + 1) % Math.max(items.length, 1));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setActive((idx - 1 + Math.max(items.length, 1)) % Math.max(items.length, 1));
            } else if (e.key === 'Enter') {
              e.preventDefault();
              items[idx]?.run();
            }
          }}
        />
        {teamKey && (
          <div className={styles.scope}>
            <span role="status" aria-live="polite">
              {allTeams ? 'Searching all teams' : `Searching ${teamKey} only`}
              {!allTeams && ' (ticket numbers search all teams)'}
            </span>
            <button
              type="button"
              className={styles.scopeButton}
              aria-pressed={allTeams}
              onClick={() => {
                setAllTeams(!allTeams);
                setActive(0);
                inputRef.current?.focus();
              }}
            >
              All teams
            </button>
          </div>
        )}
        <ul id={listId} role="listbox" aria-label="Results" className={styles.list}>
          {items.map((it, i) => {
            const head = it.group !== lastGroup;
            lastGroup = it.group;
            return (
              <li key={it.id} role="presentation">
                {head && <div className={styles.group}>{it.group}</div>}
                <div
                  id={`${listId}-${i}`}
                  role="option"
                  aria-selected={i === idx}
                  className={`${ui.option} ${i === idx ? ui.optionActive : ''}`}
                  onMouseMove={() => setActive(i)}
                  onClick={it.run}
                  title={it.title}
                  aria-label={it.title}
                >
                  {it.icon}
                  <span className={ui.optionText}>{it.label}</span>
                  {it.hint && <kbd>{it.hint}</kbd>}
                </div>
              </li>
            );
          })}
          {items.length === 0 && (
            <li role="presentation" className={ui.hint} style={{ padding: 12 }}>
              {found.isFetching ? 'Searching…' : 'No results'}
              {!found.isFetching && scoped && debounced.trim() && (
                <>
                  {' '}
                  in {teamKey}.{' '}
                  <button
                    type="button"
                    className={styles.scopeButton}
                    onClick={() => {
                      setAllTeams(true);
                      inputRef.current?.focus();
                    }}
                  >
                    Search all teams
                  </button>
                </>
              )}
            </li>
          )}
        </ul>
      </div>
    </Dialog>
  );
}
