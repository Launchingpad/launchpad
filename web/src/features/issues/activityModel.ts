import type {
  ActivityEntry,
  Attachment,
  Comment,
  IssueRef,
  Relations,
  RelationType,
  StateChange,
} from '../../api/types';
import { priorityLabel } from '../../lib/priority';

/**
 * The Activity stream as data (LAUN-48, LAUN-95). Everything the server records and returns appears here:
 * creation, state changes (`stateHistory`), link and file attachments (`include=attachments`), comments, the
 * recorded field edits (`GET /issues/{issue}/activity`, LAUN-49), relations added (`createdBy`, `createdAt`)
 * and merge or pull request rows (mrLinked, mrMerged, mrClosed; LAUN-83). A kind the web does not know is still
 * shown, as "changed <kind>", never dropped. Text from the git host or from an edit (title, repoPath) stays
 * plain text: nothing here makes HTML or Markdown from it.
 */

const RELATION_TYPES: RelationType[] = ['blocks', 'blockedBy', 'related', 'duplicateOf', 'duplicates'];

/** A burst: same kind, same actor, same source, each event within this gap of the one before. */
export const FOLD_GAP_MS = 5 * 60 * 1000;

export type Actor = { id: string; username: string; name: string } | null;

export type EventItem =
  | { kind: 'created'; key: string; at: string; actor: Actor }
  | {
      kind: 'state';
      key: string;
      at: string;
      actor: Actor;
      source: string;
      from: string;
      to: string;
      /** how many moves this line holds */
      count: number;
    }
  | { kind: 'link'; key: string; at: string; actor: Actor; items: Attachment[] }
  | { kind: 'file'; key: string; at: string; actor: Actor; items: Attachment[] }
  /** A recorded field edit (LAUN-49). `text` is the sentence after the actor's name, plain text. */
  | { kind: 'edit'; key: string; at: string; actor: Actor; source: string; text: string }
  | {
      kind: 'labels';
      key: string;
      at: string;
      actor: Actor;
      source: string;
      verb: 'added' | 'removed';
      names: string[];
    }
  | { kind: 'relation'; key: string; at: string; actor: Actor; type: RelationType; items: IssueRef[] }
  | {
      kind: 'mr';
      key: string;
      at: string;
      /** null: the host's own row (the server records no actor for it) */
      actor: Actor;
      source: string;
      state: 'linked' | 'merged' | 'closed';
      provider: string;
      repoPath: string;
      number: number | null;
      url: string | null;
      title: string;
    };

export type TimelineItem = EventItem | { kind: 'comment'; key: string; at: string; comment: Comment };

const MAX_TEXT = 100;
function clip(v: string): string {
  const t = v.replace(/\s+/g, ' ').trim();
  return t.length > MAX_TEXT ? `${t.slice(0, MAX_TEXT - 1)}…` : t;
}
function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}
function field(v: unknown, k: string): string {
  return isObj(v) && typeof v[k] === 'string' ? (v[k] as string) : '';
}

type Noun =
  'title' | 'assignee' | 'priority' | 'estimate' | 'due date' | 'parent' | 'project' | 'milestone' | 'cycle';

/** The value of a snapshot as one plain phrase; '' when there is none. */
function valueText(kind: ActivityEntry['kind'], v: unknown): string {
  if (v === null || v === undefined || v === '') return '';
  switch (kind) {
    case 'title':
    case 'dueDate':
      return typeof v === 'string' ? clip(v) : '';
    case 'priority':
      return typeof v === 'number' && v !== 0 ? priorityLabel(v) : '';
    case 'estimate':
      return typeof v === 'number' ? String(v) : '';
    case 'assignee':
      return clip(field(v, 'name') || field(v, 'username'));
    case 'parent': {
      const id = field(v, 'identifier');
      const title = field(v, 'title');
      return clip([id, title].filter(Boolean).join(' '));
    }
    case 'cycle': {
      const n = isObj(v) && typeof v.number === 'number' ? `Cycle ${v.number}` : '';
      const name = field(v, 'name');
      return clip(name ? (n ? `${n}, ${name}` : name) : n);
    }
    default:
      return clip(field(v, 'name'));
  }
}

const NOUN: Partial<Record<ActivityEntry['kind'], Noun>> = {
  title: 'title',
  assignee: 'assignee',
  priority: 'priority',
  estimate: 'estimate',
  dueDate: 'due date',
  parent: 'parent',
  project: 'project',
  milestone: 'milestone',
  cycle: 'cycle',
};

function editText(a: ActivityEntry): string {
  if (a.kind === 'description') return 'edited the description';
  if (a.kind === 'relationRemoved') {
    const from = a.from;
    const other = isObj(from) && isObj(from.issue) ? field(from.issue, 'identifier') : '';
    const type = isObj(from) && typeof from.type === 'string' ? from.type : '';
    const label = RELATION_REMOVED[type];
    return label && other ? `removed the relation: ${label} ${other}` : 'removed a relation';
  }
  const noun = NOUN[a.kind];
  if (!noun) return `changed ${clip(String(a.kind))}`; // a kind this web does not know: shown, never dropped
  const from = valueText(a.kind, a.from);
  const to = valueText(a.kind, a.to);
  if (from && to) return `changed the ${noun} from ${from} to ${to}`;
  if (to) return `set the ${noun} to ${to}`;
  if (from) return `removed the ${noun} (${from})`;
  return `changed the ${noun}`;
}

/** Relation types as the issue itself sees them (relationRemoved `from.type`). */
const RELATION_REMOVED: Record<string, string> = {
  blocks: 'blocks',
  blockedBy: 'blocked by',
  related: 'related to',
  duplicateOf: 'duplicate of',
  duplicates: 'duplicated by',
};

function mrEvent(a: ActivityEntry, state: 'linked' | 'merged' | 'closed'): EventItem {
  const to = a.to;
  const n = isObj(to) && typeof to.number === 'number' ? to.number : null;
  return {
    kind: 'mr',
    key: a.id,
    at: a.createdAt,
    actor: a.actor,
    source: a.source,
    state,
    provider: field(to, 'provider') || a.source,
    repoPath: field(to, 'repoPath'),
    number: n,
    url: field(to, 'url') || null,
    title: clip(field(to, 'title')),
  };
}

/** One activity row as a timeline event. Never throws, whatever the snapshot holds. */
export function activityEvent(a: ActivityEntry): EventItem {
  switch (a.kind) {
    case 'mrLinked':
      return mrEvent(a, 'linked');
    case 'mrMerged':
      return mrEvent(a, 'merged');
    case 'mrClosed':
      return mrEvent(a, 'closed');
    case 'labelAdded':
    case 'labelRemoved': {
      const v = a.kind === 'labelAdded' ? a.to : a.from;
      return {
        kind: 'labels',
        key: a.id,
        at: a.createdAt,
        actor: a.actor,
        source: a.source,
        verb: a.kind === 'labelAdded' ? 'added' : 'removed',
        names: [clip(field(v, 'name')) || 'a label'],
      };
    }
    default:
      return {
        kind: 'edit',
        key: a.id,
        at: a.createdAt,
        actor: a.actor,
        source: a.source,
        text: editText(a),
      };
  }
}

function sameActor(a: Actor, b: Actor): boolean {
  return (a?.id ?? null) === (b?.id ?? null);
}

function ms(at: string): number {
  return Date.parse(at);
}

/** Folds neighbours of one kind (see FOLD_GAP_MS). `events` must be in time order. Pure. */
export function foldEvents(events: EventItem[]): EventItem[] {
  const out: EventItem[] = [];
  for (const e of events) {
    const prev = out[out.length - 1];
    const close = prev !== undefined && ms(e.at) - ms(prev.at) <= FOLD_GAP_MS;
    if (prev && close && prev.kind === e.kind && sameActor(prev.actor, e.actor)) {
      if (prev.kind === 'state' && e.kind === 'state' && prev.source === e.source) {
        // Backlog -> Todo -> In Progress reads "from Backlog to In Progress". The line keeps the LAST time.
        out[out.length - 1] = { ...prev, to: e.to, at: e.at, count: prev.count + 1 };
        continue;
      }
      if ((prev.kind === 'link' && e.kind === 'link') || (prev.kind === 'file' && e.kind === 'file')) {
        out[out.length - 1] = { ...prev, at: e.at, items: [...prev.items, ...e.items] };
        continue;
      }
      if (prev.kind === 'relation' && e.kind === 'relation' && prev.type === e.type) {
        out[out.length - 1] = { ...prev, at: e.at, items: [...prev.items, ...e.items] };
        continue;
      }
      if (prev.kind === 'labels' && e.kind === 'labels' && prev.verb === e.verb && prev.source === e.source) {
        out[out.length - 1] = { ...prev, at: e.at, names: [...prev.names, ...e.names] };
        continue;
      }
    }
    out.push(e);
  }
  // A fold that ends where it began (Todo, Done, Todo) says nothing: drop it.
  return out.filter((e) => !(e.kind === 'state' && e.from === e.to));
}

/**
 * Merges everything into one list in time order. A comment ends a burst: only events with nothing
 * between them are folded. Equal times keep the order: created, state changes, edits, relations, attachments, comments.
 */
export function buildTimeline(input: {
  createdAt: string;
  creator: Actor;
  history: StateChange[];
  attachments: Attachment[];
  comments: Comment[];
  /** `GET /issues/{issue}/activity` rows; absent while loading or when the read failed */
  activity?: ActivityEntry[];
  /** `include=relations`: each one is an "added" row with its `createdBy` and `createdAt` */
  relations?: Relations;
}): TimelineItem[] {
  type Raw = { at: string; rank: number; item: EventItem | { kind: 'comment'; comment: Comment } };
  const raw: Raw[] = [
    {
      at: input.createdAt,
      rank: 0,
      item: { kind: 'created', key: 'created', at: input.createdAt, actor: input.creator },
    },
  ];
  for (const h of input.history) {
    if (!h.fromStateName) continue; // the creation row: "created the issue" says it
    raw.push({
      at: h.createdAt,
      rank: 1,
      item: {
        kind: 'state',
        key: h.id,
        at: h.createdAt,
        actor: h.actor,
        source: h.source,
        from: h.fromStateName,
        to: h.toStateName,
        count: 1,
      },
    });
  }
  for (const a of input.activity ?? []) raw.push({ at: a.createdAt, rank: 2, item: activityEvent(a) });
  for (const type of RELATION_TYPES) {
    for (const r of input.relations?.[type] ?? []) {
      raw.push({
        at: r.createdAt,
        rank: 3,
        item: { kind: 'relation', key: r.id, at: r.createdAt, actor: r.createdBy, type, items: [r.issue] },
      });
    }
  }
  // The server writes a link attachment next to every mrLinked row (same URL, no creator): the MR row says it.
  const mrUrls = new Set(
    (input.activity ?? []).filter((a) => a.kind === 'mrLinked').map((a) => field(a.to, 'url')),
  );
  mrUrls.delete('');
  for (const a of input.attachments) {
    if (a.kind !== 'file' && a.url && mrUrls.has(a.url)) continue;
    raw.push({
      at: a.createdAt,
      rank: 4,
      item: {
        kind: a.kind === 'file' ? 'file' : 'link',
        key: a.id,
        at: a.createdAt,
        actor: a.creator,
        items: [a],
      },
    });
  }
  for (const c of input.comments)
    raw.push({ at: c.createdAt, rank: 5, item: { kind: 'comment', comment: c } });
  raw.sort((a, b) => ms(a.at) - ms(b.at) || a.rank - b.rank);

  const out: TimelineItem[] = [];
  let run: EventItem[] = [];
  const flush = () => {
    out.push(...foldEvents(run));
    run = [];
  };
  for (const r of raw) {
    if (r.item.kind === 'comment') {
      flush();
      out.push({
        kind: 'comment',
        key: r.item.comment.id,
        at: r.item.comment.createdAt,
        comment: r.item.comment,
      });
    } else run.push(r.item);
  }
  flush();
  return out;
}
