import { createContext, memo, useContext } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize';
import remarkParse from 'remark-parse';
import remarkRehype from 'remark-rehype';
import { unified } from 'unified';
import type { Options as SanitizeSchema } from 'rehype-sanitize';
import { apiOrigin } from '../env';
import { isTaskMarkerAt, toggleTaskAt } from './taskList';
import styles from '../styles/ui.module.css';

// User markdown is untrusted. Defence in depth:
//  1. react-markdown does not parse raw HTML (no rehype-raw), so `<script>` is text.
//  2. rehype-sanitize drops anything outside this schema (no on* attributes,
//     no style, no iframes) and only lets http, https and mailto links through.
//  3. Images render only when they come from our API origin; anything else
//     becomes a plain link, so a comment cannot make the browser call out.
const schema: SanitizeSchema = {
  ...defaultSchema,
  protocols: { ...defaultSchema.protocols, href: ['http', 'https', 'mailto'], src: ['http', 'https'] },
  attributes: {
    ...defaultSchema.attributes,
    a: [['href'], 'title'],
    img: ['src', 'alt', 'title'],
  },
};

/** Allowed image URL: our API origin, or a path on it. Never data:, blob:, javascript: or another host. */
export function isOwnImage(src: string | undefined): boolean {
  if (!src) return false;
  try {
    const u = new URL(src, apiOrigin() + '/');
    return (u.protocol === 'https:' || u.protocol === 'http:') && u.origin === apiOrigin();
  } catch {
    return false;
  }
}

/** Only these URL schemes survive; react-markdown calls this for href and src. */
export function safeUrl(url: string): string {
  try {
    const u = new URL(url, 'http://placeholder.invalid/');
    if (['http:', 'https:', 'mailto:'].includes(u.protocol)) return url;
    return '';
  } catch {
    return '';
  }
}

const components: Components = {
  a({ href, children }) {
    if (!href) return <span>{children}</span>;
    return (
      <a className="textLink" href={href} target="_blank" rel="noopener noreferrer nofollow">
        {children}
      </a>
    );
  },
  img({ src, alt }) {
    const s = typeof src === 'string' ? src : undefined;
    if (isOwnImage(s)) return <img src={s} alt={alt ?? ''} loading="lazy" referrerPolicy="no-referrer" />;
    return <span>[image: {alt || 'blocked'}]</span>;
  },
};

// Task lists (LAUN-47). react-markdown has no GFM, so "- [ ] x" arrives as a list item whose first
// text starts with "[ ] ". This plugin runs AFTER the sanitiser, so the schema above is untouched
// and only this code can make an <input>: it swaps the marker for a checkbox that remembers the
// marker's offset in the source. A marker that is escaped or not in the source stays text.
interface HNode {
  type: string;
  tagName?: string;
  value?: string;
  properties?: Record<string, unknown>;
  children?: HNode[];
  position?: { start: { offset?: number } };
}

function textOf(n: HNode): string {
  if (n.type === 'text') return n.value ?? '';
  if (n.tagName === 'ul' || n.tagName === 'ol') return '';
  return (n.children ?? []).map(textOf).join('');
}

function firstText(li: HNode): { holder: HNode; index: number } | null {
  let holder = li;
  for (;;) {
    const i = (holder.children ?? []).findIndex((c) => !(c.type === 'text' && !(c.value ?? '').trim()));
    if (i < 0) return null;
    const c = holder.children![i]!;
    if (c.type === 'text') return { holder, index: i };
    if (c.tagName === 'p' && holder === li) {
      holder = c;
      continue;
    }
    return null;
  }
}

function taskify(node: HNode, source: string): boolean {
  let any = false;
  if (node.tagName === 'li') {
    const hit = firstText(node);
    const t = hit && hit.holder.children![hit.index]!;
    const off = t?.position?.start.offset;
    const m = t && /^\[([ xX])\](?=\s|$)/.exec(t.value ?? '');
    if (hit && t && m && off !== undefined && isTaskMarkerAt(source, off)) {
      t.value = (t.value ?? '').slice(3).replace(/^\s+/, '');
      const input: HNode = {
        type: 'element',
        tagName: 'input',
        properties: {
          type: 'checkbox',
          checked: m[1] !== ' ',
          dataTaskOffset: off,
          ariaLabel: textOf(node).trim() || 'Task',
        },
        children: [],
      };
      hit.holder.children!.splice(hit.index, 0, input);
      node.properties = { ...node.properties, className: ['task-list-item'] };
      any = true;
    }
  }
  for (const c of node.children ?? [])
    if (taskify(c, source) && node.tagName && /^[uo]l$/.test(node.tagName)) any = true;
  if (any && (node.tagName === 'ul' || node.tagName === 'ol')) {
    node.properties = { ...node.properties, className: ['contains-task-list'] };
  }
  return any;
}

function rehypeTaskList() {
  return (tree: HNode, file: { value: unknown }) => {
    taskify(tree, String(file.value));
  };
}

// The boxes read the text and the handler from context, so `components` keeps one identity and a
// re-render never remounts a checkbox (a click in flight would be lost).
interface TaskCtx {
  text: string;
  onToggle?: (next: string) => void;
}
const TaskContext = createContext<TaskCtx>({ text: '' });

function TaskBox(props: { checked?: boolean; type?: string; 'aria-label'?: string }) {
  const { text, onToggle } = useContext(TaskContext);
  const raw = (props as Record<string, unknown>)['data-task-offset'];
  const offset = typeof raw === 'number' ? raw : Number(raw);
  if (props.type !== 'checkbox' || raw === undefined || !Number.isFinite(offset)) return null;
  return (
    // The label is the 24px hit area; a click anywhere in it toggles the box and never reaches the
    // click-to-edit text around it.
    <label className={styles.mdTaskHit} onClick={(e) => e.stopPropagation()}>
      <input
        type="checkbox"
        className={styles.mdTask}
        checked={!!props.checked}
        disabled={!onToggle}
        aria-label={props['aria-label']}
        onChange={() => {
          const next = toggleTaskAt(text, offset);
          if (next !== null && onToggle) onToggle(next);
        }}
      />
    </label>
  );
}

const mdComponents: Components = { ...components, input: TaskBox };

function MarkdownImpl({ text, onToggleTask }: { text: string; onToggleTask?: (next: string) => void }) {
  return (
    <div className={styles.md}>
      <TaskContext.Provider value={{ text, onToggle: onToggleTask }}>
        <ReactMarkdown
          rehypePlugins={[[rehypeSanitize, schema], rehypeTaskList]}
          urlTransform={safeUrl}
          components={mdComponents}
        >
          {text}
        </ReactMarkdown>
      </TaskContext.Provider>
    </div>
  );
}
export const Markdown = memo(MarkdownImpl);

// The images the Markdown component really renders (LAUN-115, LAUN-132). This runs the SAME unified chain as
// react-markdown's createProcessor (remark-parse, remark-rehype with allowDangerousHtml, then rehype-sanitize with
// OUR schema) and walks the hast tree, instead of rendering to static markup: `react-dom/server` stays out of the
// entry chunk. Task lists (rehypeTaskList) run after the sanitiser and touch no image, so they are not needed.
const imageProcessor = unified()
  .use(remarkParse)
  .use(remarkRehype, { allowDangerousHtml: true })
  .use(rehypeSanitize, schema);

function collectImages(node: HNode, out: string[]) {
  if (node.type === 'element' && node.tagName === 'img') {
    // react-markdown applies urlTransform to every `src`, then the `img` component shows an <img> only for an
    // own-origin URL (anything else is a "[image: ...]" span). Same two steps here.
    const raw = node.properties?.['src'];
    if (raw !== undefined) {
      const src = safeUrl(String(raw || ''));
      if (isOwnImage(src)) out.push(src);
    }
  }
  for (const c of node.children ?? []) collectImages(c, out);
}

/**
 * The `src` of every image the Markdown component really renders for this text (LAUN-115), in document order
 * (an image shown twice is listed twice). Code blocks, inline code, escaped `\![]()`, plain links, raw HTML
 * (the sanitiser drops it) and blocked URLs (off-origin, data:) give none, exactly as on screen.
 */
export function renderedImageSources(text: string): string[] {
  if (!text.includes('![')) return [];
  const out: string[] = [];
  const tree = imageProcessor.runSync(imageProcessor.parse(text)) as unknown as HNode;
  collectImages(tree, out);
  return out;
}
