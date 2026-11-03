import { useMemo, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import {
  ACTIVITY_MAX_PAGES,
  ACTIVITY_PAGE,
  useComments,
  useIssueActivity,
  useMembers,
  useTeamStates,
} from '../../api/queries';
import type { Attachment, Issue, WorkflowState } from '../../api/types';
import { providerLabel, requestNoun, requestRef } from './changeText';
import { fullDate, relativeTime } from '../../lib/time';
import { Avatar } from '../../ui/Avatar';
import { IconAttach, IconEdit, IconLink, IconMerge, IconPlus, StatusIcon } from '../../ui/Icons';
import { ErrorState, Spinner } from '../../ui/States';
import { SubscribeControl } from './SubscribeControl';
import { buildTree, CommentItem, Compose } from './Comments';
import { buildTimeline, type Actor, type EventItem } from './activityModel';
import styles from './issues.module.css';
import detail from './detail.module.css';
import ui from '../../styles/ui.module.css';

/** "GitLab", "import" and "system" are the sources the server records for a state change (api is a person or a token). */
const SOURCE_LABEL: Record<string, string> = {
  gitlab: 'GitLab',
  github: 'GitHub',
  bitbucket: 'Bitbucket',
  import: 'import',
  system: 'system',
};

/** The name for a row with no actor: its source ("GitLab", "System"), else a neutral word. */
function sourceName(source: string): string {
  const src = SOURCE_LABEL[source];
  return src ? src[0]!.toUpperCase() + src.slice(1) : 'Someone';
}

function When({ at }: { at: string }) {
  return (
    <time dateTime={at} title={fullDate(at)} className={detail.when}>
      {relativeTime(at)}
    </time>
  );
}

function StateName({ name, states }: { name: string; states: Map<string, WorkflowState> }) {
  const s = states.get(name);
  return (
    <span className={detail.stateName}>
      {s && <StatusIcon type={s.type} color={s.color} size={12} name={s.name} />}
      {name}
    </span>
  );
}

/** A folded burst (an import can make dozens) lists this many titles, then "+N more". */
const MAX_LISTED = 3;

function safeHref(url: string | null): string | null {
  return url && /^https?:\/\//i.test(url) ? url : null;
}

function AttachmentLink({ a }: { a: Attachment }) {
  const href = a.kind === 'file' ? a.downloadUrl : safeHref(a.url);
  const text = (a.kind === 'file' ? a.filename : a.title) || a.url || 'attachment';
  return href ? (
    <a className="textLink" href={href} target="_blank" rel="noreferrer noopener">
      {text}
    </a>
  ) : (
    <span>{text}</span>
  );
}

function who(actor: Actor, fallback = 'Someone') {
  return <strong>{actor?.name ?? fallback}</strong>;
}

function EventLine({
  e,
  states,
  agents,
  updatedAt,
}: {
  updatedAt: string;
  e: EventItem;
  states: Map<string, WorkflowState>;
  agents: Set<string>;
}) {
  const org = useOrgSlug();
  let icon: ReactNode;
  let text: ReactNode;
  switch (e.kind) {
    case 'created':
      icon = e.actor ? <Avatar name={e.actor.name} username={e.actor.username} /> : <IconPlus />;
      text = <span title={`Updated ${fullDate(updatedAt)}`}>{who(e.actor)} created the issue</span>;
      break;
    case 'state': {
      const to = states.get(e.to);
      icon = to ? <StatusIcon type={to.type} color={to.color} name={to.name} /> : <IconPlus />;
      const src = SOURCE_LABEL[e.source];
      text = (
        <>
          {who(e.actor, src ? src[0]!.toUpperCase() + src.slice(1) : 'Someone')} moved from{' '}
          <StateName name={e.from} states={states} /> to <StateName name={e.to} states={states} />
          {src && e.actor && <span> via {src}</span>}
          {e.actor && agents.has(e.actor.id) && e.source === 'api' && <span> via agent</span>}
        </>
      );
      break;
    }
    case 'link':
    case 'file': {
      icon = e.kind === 'link' ? <IconLink /> : <IconAttach />;
      const n = e.items.length;
      const noun = e.kind === 'link' ? 'link' : 'file';
      const verb = e.kind === 'link' ? 'linked' : 'attached';
      // An attachment records no source: with no creator (a GitLab hook, an import) no name is made up.
      const lead = e.actor ? (
        <>
          {who(e.actor)} {verb}
        </>
      ) : (
        <>{verb[0]!.toUpperCase() + verb.slice(1)}</>
      );
      const shown = e.items.slice(0, MAX_LISTED);
      text = (
        <>
          {lead}{' '}
          {n === 1 ? (
            <AttachmentLink a={e.items[0]!} />
          ) : (
            <>
              {n} {noun}s:{' '}
              {shown.map((a, i) => (
                <span key={a.id}>
                  {i > 0 && ', '}
                  <AttachmentLink a={a} />
                </span>
              ))}
              {n > shown.length && <span> +{n - shown.length} more</span>}
            </>
          )}
        </>
      );
      break;
    }
    case 'edit':
      icon = e.actor ? <Avatar name={e.actor.name} username={e.actor.username} /> : <IconEdit />;
      text = (
        <>
          {who(e.actor, sourceName(e.source))} {e.text}
        </>
      );
      break;
    case 'labels': {
      icon = <IconEdit />;
      const n = e.names.length;
      const shown = e.names.slice(0, MAX_LISTED);
      text = (
        <>
          {who(e.actor, sourceName(e.source))} {e.verb} {n === 1 ? 'the label' : `${n} labels:`}{' '}
          <span className={detail.stateName}>{shown.join(', ')}</span>
          {n > shown.length && <span> +{n - shown.length} more</span>}
        </>
      );
      break;
    }
    case 'relation': {
      icon = <IconLink />;
      const lead =
        e.type === 'blocks'
          ? 'marked this as blocking'
          : e.type === 'blockedBy'
            ? 'marked this as blocked by'
            : e.type === 'related'
              ? 'marked this as related to'
              : e.type === 'duplicateOf'
                ? 'marked this as a duplicate of'
                : 'marked this as duplicated by';
      const shown = e.items.slice(0, MAX_LISTED);
      text = (
        <>
          {e.actor ? (
            <>
              {who(e.actor)} {lead}
            </>
          ) : (
            lead[0]!.toUpperCase() + lead.slice(1)
          )}{' '}
          {shown.map((r, i) => (
            <span key={r.id}>
              {i > 0 && ', '}
              <Link to={`/${org}/issue/${r.identifier}`} className="textLink">
                {r.identifier}
              </Link>
            </span>
          ))}
          {e.items.length > shown.length && <span> +{e.items.length - shown.length} more</span>}
        </>
      );
      break;
    }
    case 'mr': {
      icon = <IconMerge />;
      const ref = requestRef(e.provider, e.number);
      const href = safeHref(e.url);
      // The title and the repository path are host text: plain text only, in a text node.
      const linkText = e.title ? `${ref} ${e.title}` : ref;
      const what = (
        <>
          {requestNoun(e.provider)}{' '}
          {href ? (
            <a className="textLink" href={href} target="_blank" rel="noreferrer noopener">
              {linkText}
            </a>
          ) : (
            <span>{linkText}</span>
          )}
        </>
      );
      const lead = who(e.actor, providerLabel(e.provider));
      text =
        e.state === 'linked' ? (
          <>
            {lead} linked {what}
          </>
        ) : (
          <>
            {lead} marked {what} as {e.state}
          </>
        );
      break;
    }
  }
  return (
    <li className={detail.event}>
      <span className={detail.gutter} aria-hidden="true">
        {icon}
      </span>
      <p className={detail.evText}>
        {text}{' '}
        <span className={detail.whenWrap}>
          <span aria-hidden="true">· </span>
          <When at={e.at} />
        </span>
      </p>
    </li>
  );
}

/**
 * One quiet timeline (LAUN-21, LAUN-48): a gutter with an icon or avatar per event and a thin line joining
 * them, bursts folded, comments as cards in the same list, then the comment box. Replies stay in their card.
 * Only what the server records is shown; the gaps are in docs/internal/web-decisions.md.
 */
export function ActivityStream({ issue }: { issue: Issue }) {
  const q = useComments(issue.identifier);
  const statesQ = useTeamStates(issue.team.key);
  const members = useMembers();
  const states = useMemo(() => new Map((statesQ.data ?? []).map((s) => [s.name, s])), [statesQ.data]);
  const agents = useMemo(
    () => new Set((members.data ?? []).filter((m) => m.isAgent).map((m) => m.id)),
    [members.data],
  );
  const history = issue.stateHistory;
  const attachments = issue.attachments;
  const act = useIssueActivity(issue.identifier);
  const relations = issue.relations;
  const activity = act.data?.items;
  const items = useMemo(() => {
    const roots = buildTree(q.data ?? []);
    const byId = new Map(roots.map((n) => [n.c.id, n]));
    return buildTimeline({
      createdAt: issue.createdAt,
      creator: issue.creator,
      history: history ?? [],
      attachments: attachments ?? [],
      comments: roots.map((n) => n.c),
      activity,
      relations,
    }).map((it) => (it.kind === 'comment' ? { ...it, node: byId.get(it.key)! } : it));
  }, [issue.creator, issue.createdAt, history, attachments, q.data, activity, relations]);

  return (
    <section aria-labelledby="act-h" className={styles.block}>
      <div className={styles.blockHead}>
        <h2 id="act-h" className={styles.h2}>
          Activity
        </h2>
        <SubscribeControl identifier={issue.identifier} />
      </div>
      {q.isError && (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load comments" />
      )}
      {act.isError && (
        <ErrorState
          error={act.error}
          onRetry={() => void act.refetch()}
          title="Could not load the recorded changes"
        />
      )}
      {act.data?.truncated && (
        <p className={ui.hint} role="status">
          The newest recorded changes are not all shown. Only the first {ACTIVITY_MAX_PAGES * ACTIVITY_PAGE}{' '}
          recorded changes are listed.
        </p>
      )}
      <ol className={`${styles.thread} ${detail.stream}`}>
        {items.map((it) =>
          it.kind === 'comment' ? (
            <CommentItem key={it.key} node={it.node} identifier={issue.identifier} agents={agents} card />
          ) : (
            <EventLine key={it.key} e={it} states={states} agents={agents} updatedAt={issue.updatedAt} />
          ),
        )}
      </ol>
      {q.isPending && <Spinner />}
      <Compose identifier={issue.identifier} label="Leave a comment" />
    </section>
  );
}
