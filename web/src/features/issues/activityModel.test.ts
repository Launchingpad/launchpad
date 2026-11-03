import { describe, expect, it } from 'vitest';
import type { Attachment, Comment, StateChange } from '../../api/types';
import { buildTimeline, FOLD_GAP_MS, foldEvents, type Actor, type EventItem } from './activityModel';

const chen: Actor = { id: 'u1', username: 'chen', name: 'Chen' };
const claude: Actor = { id: 'u2', username: 'claude', name: 'Claude' };
const t = (min: number) => new Date(Date.parse('2026-10-01T00:00:00Z') + min * 60_000).toISOString();

function move(
  id: string,
  min: number,
  from: string,
  to: string,
  actor: Actor = chen,
  source = 'api',
): StateChange {
  return {
    id,
    fromStateId: 'a',
    fromStateName: from,
    toStateId: 'b',
    toStateName: to,
    actor,
    source,
    createdAt: t(min),
  } as StateChange;
}
function link(id: string, min: number, actor: Actor = chen, kind: 'link' | 'file' = 'link'): Attachment {
  return {
    id,
    issueId: 'i1',
    kind,
    title: id,
    subtitle: '',
    url: kind === 'link' ? `https://x/${id}` : null,
    filename: kind === 'file' ? id : null,
    contentType: null,
    sizeBytes: null,
    downloadUrl: null,
    creator: actor,
    createdAt: t(min),
  } as Attachment;
}
function comment(id: string, min: number): Comment {
  return {
    id,
    issueId: 'i1',
    documentId: null,
    parentId: null,
    body: id,
    author: chen,
    createdAt: t(min),
    updatedAt: t(min),
    editedAt: null,
  };
}
const base = {
  createdAt: t(0),
  creator: chen,
  history: [] as StateChange[],
  attachments: [] as Attachment[],
  comments: [] as Comment[],
};
const kinds = (x: { kind: string }[]) => x.map((i) => i.kind);

describe('buildTimeline', () => {
  it('starts with created and drops the creation row of the state history', () => {
    const creation = { ...move('h0', 0, 'x', 'Backlog'), fromStateName: null } as StateChange;
    const out = buildTimeline({ ...base, history: [creation] });
    expect(kinds(out)).toEqual(['created']);
  });

  it('orders by time and keeps created first on a tie', () => {
    const out = buildTimeline({
      ...base,
      history: [move('h1', 0, 'Backlog', 'Todo', claude)],
      comments: [comment('c1', 0)],
      attachments: [link('a1', 0, claude)],
    });
    expect(kinds(out)).toEqual(['created', 'state', 'link', 'comment']);
  });

  it('folds moves by one actor inside the gap into one line: first from, last to, last time', () => {
    const out = buildTimeline({
      ...base,
      history: [move('h1', 1, 'Backlog', 'Todo'), move('h2', 3, 'Todo', 'In Progress')],
    });
    expect(out).toHaveLength(2);
    expect(out[1]).toMatchObject({ kind: 'state', from: 'Backlog', to: 'In Progress', count: 2, at: t(3) });
  });

  it('does not fold moves of two actors or two sources', () => {
    const a = buildTimeline({ ...base, history: [move('h1', 1, 'A', 'B'), move('h2', 2, 'B', 'C', claude)] });
    expect(a).toHaveLength(3);
    const b = buildTimeline({
      ...base,
      history: [move('h1', 1, 'A', 'B'), move('h2', 2, 'B', 'C', chen, 'gitlab')],
    });
    expect(b).toHaveLength(3);
  });

  it('keeps the gap rule: exactly the gap folds, one minute more does not', () => {
    const gap = FOLD_GAP_MS / 60_000;
    expect(
      buildTimeline({ ...base, history: [move('h1', 1, 'A', 'B'), move('h2', 1 + gap, 'B', 'C')] }),
    ).toHaveLength(2);
    expect(
      buildTimeline({ ...base, history: [move('h1', 1, 'A', 'B'), move('h2', 2 + gap, 'B', 'C')] }),
    ).toHaveLength(3);
  });

  it('measures the gap from the previous event, so a slow chain keeps folding', () => {
    const h = [move('h1', 1, 'A', 'B'), move('h2', 5, 'B', 'C'), move('h3', 9, 'C', 'D')];
    expect(buildTimeline({ ...base, history: h })[1]).toMatchObject({ from: 'A', to: 'D', count: 3 });
  });

  it('folds links of one actor into one line, in order, and files apart from links', () => {
    const out = buildTimeline({
      ...base,
      attachments: [link('a1', 1), link('a2', 2), link('a3', 3), link('f1', 3, chen, 'file')],
    });
    expect(kinds(out)).toEqual(['created', 'link', 'file']);
    expect((out[1] as { items: Attachment[] }).items.map((a) => a.id)).toEqual(['a1', 'a2', 'a3']);
  });

  it('a comment ends a burst', () => {
    const out = buildTimeline({
      ...base,
      history: [move('h1', 1, 'A', 'B'), move('h2', 3, 'B', 'C')],
      comments: [comment('c1', 2)],
    });
    expect(kinds(out)).toEqual(['created', 'state', 'comment', 'state']);
  });

  it('an event of another kind between two links ends the burst', () => {
    const out = buildTimeline({
      ...base,
      history: [move('h1', 2, 'A', 'B')],
      attachments: [link('a1', 1), link('a2', 3)],
    });
    expect(kinds(out)).toEqual(['created', 'link', 'state', 'link']);
  });

  it('treats a missing actor as one actor (GitLab links)', () => {
    const out = buildTimeline({ ...base, attachments: [link('a1', 1, null), link('a2', 2, null)] });
    expect(kinds(out)).toEqual(['created', 'link']);
  });
});

describe('round trips', () => {
  it('drops a fold that ends where it started (A to B to A)', () => {
    const out = buildTimeline({
      ...base,
      history: [move('h1', 1, 'Todo', 'Done'), move('h2', 2, 'Done', 'Todo')],
    });
    expect(kinds(out)).toEqual(['created']);
  });
  it('drops A to B to C to A and keeps a later move out of it', () => {
    const h = [move('h1', 1, 'A', 'B'), move('h2', 2, 'B', 'C'), move('h3', 3, 'C', 'A')];
    expect(kinds(buildTimeline({ ...base, history: h }))).toEqual(['created']);
    const out = buildTimeline({ ...base, history: [...h, move('h4', 4, 'A', 'D')] });
    expect(out[1]).toMatchObject({ from: 'A', to: 'D', count: 4 });
  });
});

describe('foldEvents', () => {
  it('does not mutate its input', () => {
    const ev: EventItem[] = [
      { kind: 'state', key: '1', at: t(1), actor: chen, source: 'api', from: 'A', to: 'B', count: 1 },
      { kind: 'state', key: '2', at: t(2), actor: chen, source: 'api', from: 'B', to: 'C', count: 1 },
    ];
    foldEvents(ev);
    expect(ev[0]).toMatchObject({ to: 'B', count: 1 });
  });
});

describe('recorded activity, relations and MR rows (LAUN-95)', () => {
  const base = { createdAt: t(0), creator: chen, history: [], attachments: [], comments: [] };
  const row = (id: string, kind: string, min: number, over: Record<string, unknown> = {}) =>
    ({ id, kind, actor: chen, source: 'api', from: null, to: null, createdAt: t(min), ...over }) as never;

  it('puts edits in time order between the other events', () => {
    const out = buildTimeline({
      ...base,
      history: [move('s1', 5, 'Backlog', 'Todo')],
      activity: [row('a', 'title', 1, { from: 'A', to: 'B' }), row('b', 'description', 10)],
    });
    expect(out.map((i) => i.kind)).toEqual(['created', 'edit', 'state', 'edit']);
    expect((out[1] as { text: string }).text).toBe('changed the title from A to B');
  });

  it('never throws on a strange snapshot and keeps an unknown kind', () => {
    const out = buildTimeline({
      ...base,
      activity: [
        row('a', 'assignee', 1, { to: 'a string' }),
        row('b', 'cycle', 2, { to: { number: 'x' } }),
        row('c', 'relationRemoved', 3, { from: 7 }),
        row('d', 'mrLinked', 4, { to: null, actor: null, source: 'gitlab' }),
        row('e', 'brandNew', 5),
      ],
    });
    const texts = out.filter((i) => i.kind === 'edit').map((i) => (i as { text: string }).text);
    expect(texts).toEqual([
      'changed the assignee',
      'changed the cycle',
      'removed a relation',
      'changed brandNew',
    ]);
    expect(out.some((i) => i.kind === 'mr')).toBe(true);
  });

  it('keeps mr fields as given and takes no markup from them', () => {
    const [, mr] = buildTimeline({
      ...base,
      activity: [
        row('m', 'mrMerged', 1, {
          actor: null,
          source: 'github',
          to: { provider: 'github', repoPath: 'a/b', number: 4, url: 'https://x/4', title: '<b>t</b>' },
        }),
      ],
    });
    expect(mr).toMatchObject({
      kind: 'mr',
      state: 'merged',
      provider: 'github',
      number: 4,
      title: '<b>t</b>',
    });
  });

  it('adds one relation row per relation, with its creator, and folds a burst of one type', () => {
    const ref = (n: number) => ({ id: `i${n}`, identifier: `ORB-${n}`, title: 'x' });
    const rel = (id: string, n: number, min: number, by: Actor) => ({
      id,
      type: 'related' as const,
      issue: ref(n),
      createdAt: t(min),
      createdBy: by,
    });
    const out = buildTimeline({
      ...base,
      relations: {
        blocks: [],
        blockedBy: [],
        related: [rel('r1', 2, 1, claude), rel('r2', 3, 2, claude), rel('r3', 4, 3, null)],
        duplicateOf: [],
        duplicates: [],
      },
    });
    const rels = out.filter((i) => i.kind === 'relation');
    expect(rels).toHaveLength(2);
    expect(rels[0]).toMatchObject({ actor: claude });
    expect((rels[0] as { items: unknown[] }).items).toHaveLength(2);
    expect(rels[1]).toMatchObject({ actor: null });
  });
});

describe('an MR link is shown once (LAUN-95)', () => {
  const base = { createdAt: t(0), creator: chen, history: [], comments: [] };
  const mrRow = {
    id: 'm',
    kind: 'mrLinked',
    actor: null,
    source: 'gitlab',
    from: null,
    to: { provider: 'gitlab', repoPath: 'a/b', number: 4, url: 'https://x/4', title: 'T' },
    createdAt: t(1),
  } as never;
  const att = (id: string, url: string) => ({ ...link(id, 1, null), url });

  it('drops the hook link attachment next to an mrLinked row with the same url', () => {
    const out = buildTimeline({ ...base, activity: [mrRow], attachments: [att('a1', 'https://x/4')] });
    expect(out.map((i) => i.kind)).toEqual(['created', 'mr']);
  });
  it('keeps the attachment when there is no mrLinked row (old data)', () => {
    const out = buildTimeline({ ...base, attachments: [att('a1', 'https://x/4')] });
    expect(out.map((i) => i.kind)).toEqual(['created', 'link']);
  });
  it('keeps an attachment with another url', () => {
    const out = buildTimeline({ ...base, activity: [mrRow], attachments: [att('a1', 'https://x/other')] });
    expect(out.map((i) => i.kind)).toEqual(['created', 'mr', 'link']);
  });
  it('clips a very long title', () => {
    const long = {
      ...(mrRow as object),
      to: { ...(mrRow as { to: object }).to, title: 'x'.repeat(500) },
    } as never;
    const out = buildTimeline({ ...base, attachments: [], activity: [long] });
    expect((out[1] as { title: string }).title.length).toBeLessThanOrEqual(100);
  });
  it('does not fold label rows of different sources', () => {
    const lab = (id: string, source: string) =>
      ({
        id,
        kind: 'labelAdded',
        actor: null,
        source,
        from: null,
        to: { id, name: id },
        createdAt: t(1),
      }) as never;
    const out = buildTimeline({ ...base, attachments: [], activity: [lab('a', 'api'), lab('b', 'system')] });
    expect(out.filter((i) => i.kind === 'labels')).toHaveLength(2);
  });
});
