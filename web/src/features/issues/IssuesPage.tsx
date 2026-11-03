import { useCallback, useMemo } from 'react';
import { useParams, useSearchParams } from 'react-router-dom';
import { useAllStates, useTeams } from '../../api/queries';
import { EmptyState } from '../../ui/States';
import { IssueBrowser } from './IssueBrowser';
import { parseFilters, writeFilters, type IssueFilters } from './filters';
import { placeKey, useRememberedView } from './viewPrefs';
import { usePageTitle } from '../../lib/documentTitle';
import { useUi } from '../shell/ui-context';
import { useIsOrgAdmin } from '../settings/access';
import { TeamForm } from '../settings/TeamForm';
import { NoTeamAccess } from './NoTeamAccess';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';

export function IssuesPage({ scope }: { scope: 'all' | 'mine' | 'team' }) {
  const params = useParams();
  const [sp, setSp] = useSearchParams();
  const { openCreate } = useUi();
  const parsed = useMemo(() => parseFilters(sp), [sp]);
  const teamKey = scope === 'team' ? params.team : undefined;
  const write = useCallback(
    (f: IssueFilters) => {
      const next = writeFilters({
        ...f,
        team: teamKey ? undefined : f.team,
        assignee: scope === 'mine' ? undefined : f.assignee,
      });
      setSp(next, { replace: true });
    },
    [setSp, scope, teamKey],
  );
  // LAUN-109: the layout and the sort are remembered for each place (all issues, mine, each team).
  const place = scope === 'team' ? placeKey('team', teamKey ?? '') : scope;
  const remembered = useRememberedView(place, sp, parsed, write);
  const filters: IssueFilters = useMemo(
    () => ({
      ...remembered.filters,
      team: teamKey ?? remembered.filters.team,
      assignee: scope === 'mine' ? 'me' : remembered.filters.assignee,
    }),
    [remembered.filters, scope, teamKey],
  );
  const setFilters = remembered.setFilters;

  const { teams } = useAllStates();
  const teamsQuery = useTeams();
  const isAdmin = useIsOrgAdmin();
  const noTeams = teamsQuery.isSuccess && teamsQuery.data.length === 0;
  const heading =
    scope === 'mine'
      ? 'My issues'
      : scope === 'team'
        ? (teams.find((t) => t.key.toLowerCase() === teamKey?.toLowerCase())?.name ?? teamKey ?? 'Team')
        : 'All issues';

  // A team page whose key matches no team, once the teams have loaded, is Not found.
  const unknownTeam =
    scope === 'team' &&
    teamsQuery.isSuccess &&
    !teamsQuery.data.some((t) => t.key.toLowerCase() === teamKey?.toLowerCase());
  usePageTitle(unknownTeam ? 'Not found' : scope === 'all' ? 'Issues' : heading);

  // A hidden team (decisions-T20 #72) is not in the list either, so it reads the same as a missing one.
  if (unknownTeam) return <EmptyState title="Team not found" />;

  return (
    <IssueBrowser
      filters={filters}
      onChange={setFilters}
      onResetView={remembered.customised ? remembered.resetView : undefined}
      columns={remembered.columns}
      onColumnsChange={remembered.setColumns}
      title={<h1 className={styles.h1}>{heading}</h1>}
      lockAssignee={scope === 'mine'}
      lockTeam={teamKey}
      blocked={
        noTeams ? (
          // an issue needs a team. A new org (invite or sign-up) can have none. An admin sees every team (T20 #2),
          // so for an admin the org has none. A member cannot tell an empty org from no access (T20 #73), and
          // gets one text for both, with no "request access" control (#77).
          isAdmin ? (
            <EmptyState title="This organisation has no team yet">
              <p>An issue needs a team. Create the first team here.</p>
              <TeamForm />
            </EmptyState>
          ) : (
            <NoTeamAccess />
          )
        ) : undefined
      }
      emptyTitle="No issues yet"
      emptyNone={
        <button
          type="button"
          className={`${ui.btn} ${ui.primary}`}
          onClick={() => openCreate({ team: teamKey })}
        >
          Create the first issue
        </button>
      }
    />
  );
}
