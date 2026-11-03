import { useId, useRef, useState, type KeyboardEvent } from 'react';
import { ApiError } from '../../api/errors';
import type { Project } from '../../api/types';
import { useUpdateProject } from './queries';
import ui from '../../styles/ui.module.css';
import styles from './projects.module.css';

/** The server's limit (decisions-T41): one emoji of at most 32 bytes. */
export const EMOJI_MAX_BYTES = 32;

/** The common emoji of the grid: each is one emoji of at most 32 bytes; the name is the button's label. */
export const COMMON_EMOJI: readonly { emoji: string; name: string }[] = [
  { emoji: '🚀', name: 'Rocket' },
  { emoji: '🎯', name: 'Target' },
  { emoji: '⭐', name: 'Star' },
  { emoji: '🔥', name: 'Fire' },
  { emoji: '💡', name: 'Light bulb' },
  { emoji: '🐛', name: 'Bug' },
  { emoji: '🛠️', name: 'Tools' },
  { emoji: '📦', name: 'Package' },
  { emoji: '📱', name: 'Mobile phone' },
  { emoji: '💻', name: 'Laptop' },
  { emoji: '🌐', name: 'Globe' },
  { emoji: '🔒', name: 'Lock' },
  { emoji: '📈', name: 'Chart going up' },
  { emoji: '🧪', name: 'Test tube' },
  { emoji: '🎨', name: 'Palette' },
  { emoji: '📝', name: 'Memo' },
  { emoji: '📚', name: 'Books' },
  { emoji: '🗓️', name: 'Calendar' },
  { emoji: '✅', name: 'Check mark' },
  { emoji: '⚡', name: 'Lightning' },
  { emoji: '🌱', name: 'Seedling' },
  { emoji: '🏁', name: 'Finish flag' },
  { emoji: '🤖', name: 'Robot' },
  { emoji: '❤️', name: 'Heart' },
];

/** Buttons per row of the grid; the arrow keys Up and Down move by this many. */
const COLUMNS = 8;

const RULE = 'Use one emoji.';

/**
 * A quick check before the call: one grapheme, at most 32 bytes, no control character, and a pictograph,
 * a flag letter or a keycap in it. The server makes the real check (a refusal there is a fixed sentence).
 */
export function looksLikeOneEmoji(v: string): boolean {
  if (!v || new TextEncoder().encode(v).length > EMOJI_MAX_BYTES) return false;
  if (/\p{Cc}/u.test(v)) return false;
  if (!/\p{Extended_Pictographic}|\p{Regional_Indicator}|⃣/u.test(v)) return false;
  if (typeof Intl !== 'undefined' && 'Segmenter' in Intl) {
    const seg = new Intl.Segmenter(undefined, { granularity: 'grapheme' });
    if ([...seg.segment(v)].length !== 1) return false;
  }
  return true;
}

/** A fixed sentence for a failed save. No server text reaches the screen. */
export function projectEmojiErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not save the emoji. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return `The server did not accept this emoji. ${RULE}`;
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You need write access to change the emoji.';
    case 404:
      return 'The server cannot find the project. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return 'Could not save the emoji. Try again.';
  }
}

/**
 * The Emoji section of the project page (LAUN-232): a grid of common emoji, a text field to paste any one
 * emoji, and a remove button. The logo, when there is one, still shows first; the emoji shows when there is
 * no logo. Like the logo field, it shows to everyone; a refusal is a fixed sentence.
 */
export function ProjectEmojiField({ project }: { project: Project }) {
  // Keyed by id: the text, status and error of one project never show on another.
  return <EmojiField key={project.id} project={project} />;
}

function EmojiField({ project }: { project: Project }) {
  const save = useUpdateProject(project.id);
  const ids = useId();
  const [text, setText] = useState('');
  const [problem, setProblem] = useState<string | null>(null);
  const [status, setStatus] = useState('');
  const current = project.emoji ?? null;
  const pressedAt = COMMON_EMOJI.findIndex((c) => c.emoji === current);
  const [focusAt, setFocusAt] = useState(pressedAt >= 0 ? pressedAt : 0);
  const buttons = useRef<(HTMLButtonElement | null)[]>([]);
  const busy = save.isPending;

  async function apply(next: string | null) {
    if (busy) return;
    setProblem(null);
    setStatus('');
    try {
      await save.mutateAsync({ emoji: next });
      setStatus(next ? 'Emoji saved.' : 'Emoji removed.');
      if (next) setText('');
    } catch (err) {
      setProblem(projectEmojiErrorText(err));
    }
  }

  function submitPasted() {
    const v = text.trim();
    if (!looksLikeOneEmoji(v)) {
      setStatus('');
      setProblem(`Enter exactly one emoji. ${RULE}`);
      return;
    }
    void apply(v);
  }

  // Roving focus: one tab stop for the grid; the arrow keys, Home and End move inside it.
  function onGridKey(e: KeyboardEvent<HTMLDivElement>) {
    const last = COMMON_EMOJI.length - 1;
    const moves: Record<string, number> = {
      ArrowRight: Math.min(last, focusAt + 1),
      ArrowLeft: Math.max(0, focusAt - 1),
      ArrowDown: Math.min(last, focusAt + COLUMNS),
      ArrowUp: Math.max(0, focusAt - COLUMNS),
      Home: 0,
      End: last,
    };
    const to = moves[e.key];
    if (to === undefined) return;
    e.preventDefault();
    setFocusAt(to);
    buttons.current[to]?.focus();
  }

  return (
    <section aria-labelledby={`${ids}-h`} className={styles.logoSection}>
      <h2 id={`${ids}-h`} className={styles.h2}>
        Emoji
      </h2>
      <div className={styles.logoBody}>
        <span className={styles.muted} id={`${ids}-help`}>
          The project shows its emoji when it has no logo.{' '}
          {current ? (
            <>
              Current emoji: <span className={styles.emojiCurrent}>{current}</span>
            </>
          ) : (
            'No emoji is set.'
          )}
        </span>
        <div
          role="group"
          aria-label="Common emoji"
          aria-describedby={`${ids}-help`}
          className={styles.emojiGrid}
          onKeyDown={onGridKey}
        >
          {COMMON_EMOJI.map((c, i) => (
            <button
              key={c.emoji}
              ref={(el) => {
                buttons.current[i] = el;
              }}
              type="button"
              className={styles.emojiButton}
              aria-label={c.name}
              aria-pressed={c.emoji === current}
              aria-disabled={busy || undefined}
              tabIndex={i === focusAt ? 0 : -1}
              onFocus={() => setFocusAt(i)}
              onClick={() => void apply(c.emoji)}
            >
              <span aria-hidden="true">{c.emoji}</span>
            </button>
          ))}
        </div>
        <form
          className={styles.emojiPaste}
          aria-label="Paste an emoji"
          onSubmit={(e) => {
            e.preventDefault();
            submitPasted();
          }}
        >
          <label htmlFor={`${ids}-in`} className={ui.label}>
            Or paste one emoji
          </label>
          <div className={styles.logoActions}>
            <input
              id={`${ids}-in`}
              className={`${ui.input} ${styles.emojiInput}`}
              value={text}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setText(e.target.value)}
            />
            <button type="submit" className={`${ui.btn} ${ui.primary}`} aria-disabled={busy || undefined}>
              Use emoji
            </button>
            {current && (
              <button
                type="button"
                className={ui.btn}
                aria-disabled={busy || undefined}
                onClick={() => void apply(null)}
              >
                Remove emoji
              </button>
            )}
          </div>
        </form>
        {problem && (
          <p role="alert" className={styles.logoProblem}>
            {problem}
          </p>
        )}
        <p role="status" className={styles.logoOk}>
          {status}
        </p>
      </div>
    </section>
  );
}
