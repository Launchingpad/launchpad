import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { keys } from '../../api/keys';
import { useOrgSlug } from '../../api/org';
import { useCreateComment, useDeleteComment, useEditComment } from '../../api/queries';
import { ApiError, errorMessage } from '../../api/errors';
import type { Comment } from '../../api/types';
import { relativeTime, fullDate } from '../../lib/time';
import { uuid } from '../../lib/uuid';
import { Markdown } from '../../lib/markdown';
import { Avatar } from '../../ui/Avatar';
import { useAuthedSession } from '../auth/session';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';
import detail from './detail.module.css';

export interface Node {
  c: Comment;
  children: Node[];
}

export function buildTree(list: Comment[]): Node[] {
  const byId = new Map<string, Node>();
  for (const c of list) byId.set(c.id, { c, children: [] });
  const roots: Node[] = [];
  for (const c of list) {
    const n = byId.get(c.id)!;
    const parent = c.parentId ? byId.get(c.parentId) : undefined;
    if (parent) parent.children.push(n);
    else roots.push(n);
  }
  return roots;
}

export function Compose({
  identifier,
  parentId,
  onDone,
  autoFocus,
  label,
}: {
  identifier: string;
  parentId?: string;
  onDone?: () => void;
  autoFocus?: boolean;
  label: string;
}) {
  const create = useCreateComment(identifier);
  const [text, setText] = useState('');
  const [error, setError] = useState<string | null>(null);
  const key = useRef(uuid());
  const failed = useRef(false);
  async function submit() {
    if (!text.trim()) return;
    setError(null);
    try {
      await create.mutateAsync({ body: text.trim(), parentId, idempotencyKey: key.current });
      key.current = uuid();
      failed.current = false;
      setText('');
      onDone?.();
    } catch (e) {
      failed.current = true; // a retry of the SAME text may reuse the key; changed text may not
      setError(errorMessage(e));
    }
  }
  return (
    <form
      className={`${styles.compose} ${detail.inputCard}`}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <label className="sr-only" htmlFor={`compose-${parentId ?? 'root'}`}>
        {label}
      </label>
      <textarea
        id={`compose-${parentId ?? 'root'}`}
        className={ui.textarea}
        placeholder={label}
        value={text}
        autoFocus={autoFocus}
        onChange={(e) => {
          if (failed.current) {
            key.current = uuid();
            failed.current = false;
          }
          setText(e.target.value);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            void submit();
          }
        }}
      />
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      <div className={styles.composeBtns}>
        {onDone && (
          <button type="button" className={ui.btn} onClick={onDone}>
            Cancel
          </button>
        )}
        <button
          type="submit"
          className={`${ui.btn} ${ui.primary}`}
          disabled={create.isPending || !text.trim()}
        >
          {create.isPending ? 'Sending…' : parentId ? 'Reply' : 'Comment'}
        </button>
      </div>
    </form>
  );
}

/**
 * A top-level comment is a card in the Activity timeline (LAUN-48): a gutter cell for the line, then the
 * card. A reply keeps the plain nested item.
 */
function ItemShell({ card, children }: { card?: boolean; children: ReactNode }) {
  if (!card) return <li className={styles.comment}>{children}</li>;
  return (
    <li className={detail.commentItem}>
      <span className={detail.gutter} aria-hidden="true" />
      <div className={detail.card}>{children}</div>
    </li>
  );
}

export function CommentItem({
  node,
  identifier,
  agents,
  card,
}: {
  node: Node;
  identifier: string;
  /** ids of agent users: their comments say so (the comment itself does not record how it was made) */
  agents?: Set<string>;
  card?: boolean;
}) {
  const { user } = useAuthedSession();
  const { c } = node;
  const [mode, setMode] = useState<'view' | 'edit' | 'reply' | 'delete'>('view');
  const [draft, setDraft] = useState(c.body);
  const [error, setError] = useState<string | null>(null);
  const edit = useEditComment(identifier);
  const del = useDeleteComment(identifier);
  const mine = c.author?.id === user.id;
  // A task-box click shows at once; kept until the refetched comment carries the same text.
  // `base` is the stored body it was made from: a different stored body (ours landed, or another
  // edit arrived) ends it, whatever the server kept. It also ends when the save settles.
  const qc = useQueryClient();
  const org = useOrgSlug();
  const [pending, setPending] = useState<{ text: string; base: string } | null>(null);
  const toggling = useRef(false);
  useEffect(() => {
    if (pending !== null && c.body !== pending.base) setPending(null);
  }, [c.body, pending]);
  const shownBody = pending?.text ?? c.body;

  return (
    <ItemShell card={card}>
      <div className={styles.commentHead}>
        <Avatar name={c.author?.name ?? '?'} username={c.author?.username} />
        <strong>{c.author?.name ?? 'Deleted user'}</strong>
        {c.author && agents?.has(c.author.id) && <span className={ui.hint}>via agent</span>}
        <time dateTime={c.createdAt} title={fullDate(c.createdAt)} className={ui.hint}>
          {relativeTime(c.createdAt)}
        </time>
        {c.editedAt && <span className={ui.hint}>(edited)</span>}
      </div>
      {mode === 'edit' ? (
        <form
          onSubmit={async (e) => {
            e.preventDefault();
            setError(null);
            try {
              await edit.mutateAsync({ id: c.id, body: draft.trim() });
              setMode('view');
            } catch (err) {
              setError(errorMessage(err));
            }
          }}
        >
          <label className="sr-only" htmlFor={`edit-${c.id}`}>
            Edit comment
          </label>
          <textarea
            id={`edit-${c.id}`}
            className={ui.textarea}
            value={draft}
            autoFocus
            onChange={(e) => setDraft(e.target.value)}
          />
          {error && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {error}
            </p>
          )}
          <div className={styles.composeBtns}>
            <button
              type="button"
              className={ui.btn}
              onClick={() => {
                setMode('view');
                setDraft(c.body);
              }}
            >
              Cancel
            </button>
            <button
              type="submit"
              className={`${ui.btn} ${ui.primary}`}
              disabled={edit.isPending || !draft.trim()}
            >
              Save
            </button>
          </div>
        </form>
      ) : (
        <Markdown
          text={shownBody}
          onToggleTask={
            mine
              ? async (next) => {
                  if (toggling.current) {
                    setError('Still saving the last change. Try again in a moment.');
                    return;
                  }
                  toggling.current = true;
                  setError(null);
                  setPending({ text: next, base: c.body });
                  try {
                    await edit.mutateAsync({ id: c.id, body: next });
                    await qc.invalidateQueries({ queryKey: keys.comments(org, identifier) });
                  } catch (err) {
                    setError(errorMessage(err));
                  } finally {
                    toggling.current = false;
                    setPending(null);
                  }
                }
              : undefined
          }
        />
      )}
      {mode === 'view' && (
        <div className={styles.commentActions}>
          {!card && (
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost}`}
              aria-label={`Reply to ${c.author?.name ?? 'Deleted user'}`}
              onClick={() => setMode('reply')}
            >
              Reply
            </button>
          )}
          {mine && (
            <>
              <button
                type="button"
                className={`${ui.btn} ${ui.ghost}`}
                onClick={() => {
                  setDraft(shownBody);
                  setMode('edit');
                }}
              >
                Edit
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.ghost} ${ui.danger}`}
                onClick={() => setMode('delete')}
              >
                Delete
              </button>
            </>
          )}
        </div>
      )}
      {mode === 'delete' && (
        <div className={ui.banner} role="alertdialog" aria-label="Confirm delete">
          <span>Delete this comment{node.children.length ? ' and its replies' : ''}?</span>
          <button
            type="button"
            className={`${ui.btn} ${ui.danger}`}
            disabled={del.isPending}
            onClick={async () => {
              setError(null);
              try {
                await del.mutateAsync(c.id);
              } catch (err) {
                setError(
                  err instanceof ApiError && err.status === 403
                    ? 'You can only delete a comment whose replies are all yours.'
                    : errorMessage(err),
                );
                setMode('view');
              }
            }}
          >
            Delete
          </button>
          <button type="button" className={ui.btn} onClick={() => setMode('view')}>
            Keep
          </button>
        </div>
      )}
      {mode !== 'edit' && error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      {mode === 'reply' && (
        <Compose
          identifier={identifier}
          parentId={c.id}
          onDone={() => setMode('view')}
          autoFocus
          label="Write a reply"
        />
      )}
      {node.children.length > 0 && (
        <ul className={styles.thread}>
          {node.children.map((ch) => (
            <CommentItem key={ch.c.id} node={ch} identifier={identifier} agents={agents} />
          ))}
        </ul>
      )}
      {card && mode === 'view' && (
        <button
          type="button"
          className={detail.replyRow}
          aria-label={`Leave a reply to ${c.author?.name ?? 'Deleted user'}`}
          onClick={() => setMode('reply')}
        >
          Leave a reply…
        </button>
      )}
    </ItemShell>
  );
}
