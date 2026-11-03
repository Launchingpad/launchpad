import { Link, Navigate, useParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useCycles, useTeams } from '../../api/queries';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { sectionCycles } from './cycles';
import { useNow } from './useNow';

/**
 * `/team/:team/cycles/current` and `/cycles/upcoming` (the sidebar children): redirect to the running
 * cycle, or to the next one that starts. The redirect replaces this entry, so Back skips it.
 */
export function CycleJump({ which }: { which: 'current' | 'upcoming' }) {
  const org = useOrgSlug();
  const { team = '' } = useParams();
  const teams = useTeams();
  const teamRow = teams.data?.find((t) => t.key.toLowerCase() === team.toLowerCase());
  const cycles = useCycles(teamRow?.key, !!teamRow?.cyclesEnabled);
  const now = useNow();
  const listUrl = `/${org}/team/${teamRow?.key ?? team}/cycles`;

  if (teams.isPending || (teamRow?.cyclesEnabled && (cycles.isPending || cycles.isFetching)))
    return <SkeletonRows />;
  if (teams.isError)
    return (
      <ErrorState error={teams.error} onRetry={() => void teams.refetch()} title="Could not load teams" />
    );
  if (cycles.isError)
    return (
      <ErrorState error={cycles.error} onRetry={() => void cycles.refetch()} title="Could not load cycles" />
    );
  if (!teamRow) return <EmptyState title="Team not found" />;
  if (!teamRow.cyclesEnabled) return <EmptyState title="Cycles are off for this team" />;
  const sec = sectionCycles(cycles.data ?? [], now);
  const target = which === 'current' ? sec.current[0] : sec.next[0];
  if (target) return <Navigate replace to={`/${org}/team/${teamRow.key}/cycle/${target.number}`} />;
  return (
    <EmptyState title={which === 'current' ? 'No current cycle' : 'No upcoming cycle'}>
      <p>
        <Link to={listUrl}>All cycles</Link>
      </p>
    </EmptyState>
  );
}
