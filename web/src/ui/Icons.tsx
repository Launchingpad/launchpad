import type { SVGProps } from 'react';
import type { StateType } from '../api/types';
import { useLook, type Look } from '../lib/theme';
import ui from '../styles/ui.module.css';

type P = SVGProps<SVGSVGElement>;
const base = (p: P): P => ({
  width: 16,
  height: 16,
  viewBox: '0 0 16 16',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.6,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
  'aria-hidden': true,
  focusable: false,
  ...p,
});

export const IconPlus = (p: P) => (
  <svg {...base(p)}>
    <path d="M8 3v10M3 8h10" />
  </svg>
);
export const IconSearch = (p: P) => (
  <svg {...base(p)}>
    <circle cx="7" cy="7" r="4.5" />
    <path d="m10.5 10.5 3 3" />
  </svg>
);
export const IconMenu = (p: P) => (
  <svg {...base(p)}>
    <path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11" />
  </svg>
);
export const IconClose = (p: P) => (
  <svg {...base(p)}>
    <path d="m4 4 8 8M12 4l-8 8" />
  </svg>
);
export const IconChevron = (p: P) => (
  <svg {...base(p)}>
    <path d="m4 6 4 4 4-4" />
  </svg>
);
export const IconCheck = (p: P) => (
  <svg {...base(p)}>
    <path d="m3.5 8.5 3 3 6-7" />
  </svg>
);
export const IconList = (p: P) => (
  <svg {...base(p)}>
    <path d="M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01" />
  </svg>
);
export const IconBoard = (p: P) => (
  <svg {...base(p)}>
    <rect x="2.5" y="2.5" width="3.5" height="11" rx="1" />
    <rect x="8" y="2.5" width="3.5" height="7" rx="1" />
  </svg>
);
export const IconSun = (p: P) => (
  <svg {...base(p)}>
    <circle cx="8" cy="8" r="2.8" />
    <path d="M8 1.5v1.6M8 12.9v1.6M1.5 8h1.6M12.9 8h1.6M3.4 3.4l1.1 1.1M11.5 11.5l1.1 1.1M3.4 12.6l1.1-1.1M11.5 4.5l1.1-1.1" />
  </svg>
);
export const IconMoon = (p: P) => (
  <svg {...base(p)}>
    <path d="M13 9.5A5.5 5.5 0 0 1 6.5 3 5.5 5.5 0 1 0 13 9.5Z" />
  </svg>
);
export const IconUser = (p: P) => (
  <svg {...base(p)}>
    <circle cx="8" cy="5.5" r="2.5" />
    <path d="M3 13.5c.6-2.4 2.6-3.5 5-3.5s4.4 1.1 5 3.5" />
  </svg>
);
export const IconInbox = (p: P) => (
  <svg {...base(p)}>
    <path d="M2.5 9.5 4 3.5h8l1.5 6v3h-11zM2.5 9.5h3.2l.8 1.5h3l.8-1.5h3.2" />
  </svg>
);
export const IconBell = (p: P) => (
  <svg {...base(p)}>
    <path d="M3.5 11.5h9l-1.2-1.8V7a3.3 3.3 0 0 0-6.6 0v2.7zM6.5 13.5a1.6 1.6 0 0 0 3 0" />
  </svg>
);
export const IconArchive = (p: P) => (
  <svg {...base(p)}>
    <path d="M2.5 3.5h11v3h-11zM3.5 6.5v6h9v-6M6.5 9h3" />
  </svg>
);
export const IconDot = (p: P) => (
  <svg {...base(p)}>
    <circle cx="8" cy="8" r="3" fill="currentColor" />
  </svg>
);
export const IconTag = (p: P) => (
  <svg {...base(p)}>
    <path d="M2.5 8.2V3.2a.7.7 0 0 1 .7-.7h5l5.3 5.3a.7.7 0 0 1 0 1l-4.7 4.7a.7.7 0 0 1-1 0z" />
    <circle cx="5.6" cy="5.6" r=".6" />
  </svg>
);
export const IconCycle = (p: P) => (
  <svg {...base(p)}>
    <path d="M13 8a5 5 0 0 1-8.7 3.4M3 8a5 5 0 0 1 8.7-3.4" />
    <path d="M12.5 2v2.8H9.7M3.5 14v-2.8h2.8" />
  </svg>
);
export const IconParent = (p: P) => (
  <svg {...base(p)}>
    <path d="M3 3v5.5a2 2 0 0 0 2 2h7M9.5 7.5l2.8 3-2.8 3" />
  </svg>
);
export const IconMore = (p: P) => (
  <svg {...base(p)}>
    <path d="M3.5 8h.01M8 8h.01M12.5 8h.01" strokeWidth="2.4" />
  </svg>
);
export const IconExpand = (p: P) => (
  <svg {...base(p)}>
    <path d="M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5 9 7M2.5 13.5 7 9" />
  </svg>
);
export const IconStack = (p: P) => (
  <svg {...base(p)}>
    <path d="m8 2.5 6 3-6 3-6-3zM2 8.5l6 3 6-3M2 11.5l6 3 6-3" />
  </svg>
);

/**
 * LAUN-235: the state colour family the Paper and Terminal themes use (their token sheets colour a state by
 * kind, not by the team's own colour). Review and testing are `started` states; the name tells them apart.
 */
export type StateKind =
  'backlog' | 'todo' | 'progress' | 'review' | 'testing' | 'done' | 'canceled' | 'triage';
export function stateKind(type: StateType, name = ''): StateKind {
  switch (type) {
    case 'backlog':
      return 'backlog';
    case 'unstarted':
      return 'todo';
    case 'completed':
      return 'done';
    case 'canceled':
      return 'canceled';
    case 'triage':
      return 'triage';
    default:
      if (/review/i.test(name)) return 'review';
      if (/test|\bqa\b|verif/i.test(name)) return 'testing';
      return 'progress';
  }
}
/** The Terminal theme's bracket tag text per kind (four characters: the words carry the state). */
export const STATE_TAG: Record<StateKind, string> = {
  backlog: 'BKLG',
  todo: 'TODO',
  progress: 'WIP ',
  review: 'REVW',
  testing: 'TEST',
  done: 'DONE',
  canceled: 'CNCL',
  triage: 'NEW ',
};
const kindColor = (k: StateKind) => `var(--state-${k === 'triage' ? 'todo' : k})`;

/**
 * Workflow status glyph: shape from the state type, colour from the state. In the Paper theme a thin ring in
 * the theme's state colour; in the Terminal theme a bracket tag such as [WIP ]. Always aria-hidden: the state
 * name is next to it or in an sr-only span, in every theme.
 */
export function StatusIcon({
  type,
  color,
  size = 14,
  name,
  look: forced,
}: {
  type: StateType;
  color: string;
  size?: number;
  /** The state name: tells review and testing apart in the Paper and Terminal themes. */
  name?: string;
  /** Draw in this theme instead of the page's (the theme previews on the Appearance page). */
  look?: Look;
}) {
  const pageLook = useLook();
  const look = forced ?? pageLook;
  if (look === 'terminal') {
    const k = stateKind(type, name);
    return (
      <span className={ui.stateTag} aria-hidden="true" data-state-tag={k} style={{ color: kindColor(k) }}>
        [{STATE_TAG[k]}]
      </span>
    );
  }
  if (look === 'paper') return <PaperStateGlyph kind={stateKind(type, name)} size={size} />;
  return <LinearStatusIcon type={type} color={color} size={size} />;
}

function PaperStateGlyph({ kind, size }: { kind: StateKind; size: number }) {
  const dashed = kind === 'backlog' || kind === 'canceled' || kind === 'triage';
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 14 14"
      aria-hidden="true"
      focusable="false"
      data-state-glyph={kind}
      style={{ color: kindColor(kind), flex: 'none' }}
    >
      <circle
        cx="7"
        cy="7"
        r="5.8"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeDasharray={dashed ? (kind === 'triage' ? '1.6 2.2' : '2 2') : undefined}
      />
      {(kind === 'progress' || kind === 'review' || kind === 'testing') && (
        <circle cx="7" cy="7" r="2.6" fill="currentColor" />
      )}
      {kind === 'done' && (
        <path
          d="m4.4 7.2 1.8 1.8 3.4-3.8"
          stroke="currentColor"
          strokeWidth="1.5"
          fill="none"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      )}
      {kind === 'canceled' && (
        <path d="m5 5 4 4M9 5 5 9" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
      )}
    </svg>
  );
}

function LinearStatusIcon({ type, color, size = 14 }: { type: StateType; color: string; size?: number }) {
  const common = {
    width: size,
    height: size,
    viewBox: '0 0 14 14',
    'aria-hidden': true,
    focusable: false,
  } as const;
  const c = color || 'currentColor';
  switch (type) {
    case 'completed':
      return (
        <svg {...common}>
          <circle cx="7" cy="7" r="6" fill={c} />
          <path
            d="m4.2 7.2 2 2 3.6-4"
            stroke="#fff"
            strokeWidth="1.5"
            fill="none"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      );
    case 'canceled':
      return (
        <svg {...common}>
          <circle cx="7" cy="7" r="6" fill={c} />
          <path d="m4.7 4.7 4.6 4.6M9.3 4.7 4.7 9.3" stroke="#fff" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case 'started':
      return (
        <svg {...common}>
          <circle cx="7" cy="7" r="5.8" fill="none" stroke={c} strokeWidth="1.5" />
          <path d="M7 7V3.2a3.8 3.8 0 0 1 0 7.6Z" fill={c} />
        </svg>
      );
    case 'unstarted':
      return (
        <svg {...common}>
          <circle cx="7" cy="7" r="5.8" fill="none" stroke={c} strokeWidth="1.5" />
        </svg>
      );
    case 'triage':
      return (
        <svg {...common}>
          <circle cx="7" cy="7" r="5.8" fill="none" stroke={c} strokeWidth="1.5" strokeDasharray="2 2" />
        </svg>
      );
    default:
      return (
        <svg {...common}>
          <circle cx="7" cy="7" r="5.8" fill="none" stroke={c} strokeWidth="1.5" strokeDasharray="1.6 2.2" />
        </svg>
      );
  }
}

/** Priority bars. 0 = none (dashes), 1 = urgent. */
export function PriorityIcon({ priority, size = 14 }: { priority: number; size?: number }) {
  const common = {
    width: size,
    height: size,
    viewBox: '0 0 14 14',
    'aria-hidden': true,
    focusable: false,
  } as const;
  if (priority === 1) {
    return (
      <svg {...common}>
        <rect x="1" y="1" width="12" height="12" rx="3" fill="#d6481f" />
        <path d="M7 3.6v4.2M7 9.6v.8" stroke="#fff" strokeWidth="1.6" strokeLinecap="round" />
      </svg>
    );
  }
  if (priority === 0) {
    return (
      <svg {...common}>
        <path
          d="M2 7h2.4M5.8 7h2.4M9.6 7H12"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          opacity="0.6"
        />
      </svg>
    );
  }
  const filled = priority === 2 ? 3 : priority === 3 ? 2 : 1;
  return (
    <svg {...common}>
      {[0, 1, 2].map((i) => (
        <rect
          key={i}
          x={2 + i * 4}
          y={9 - i * 3}
          width="2.4"
          height={3 + i * 3}
          rx="0.8"
          fill="currentColor"
          opacity={i < filled ? 1 : 0.28}
        />
      ))}
    </svg>
  );
}

export const IconProject = (p: P) => (
  <svg {...base(p)}>
    <path d="M8 2 13.5 5v6L8 14l-5.5-3V5z" />
    <path d="M2.5 5 8 8l5.5-3M8 8v6" />
  </svg>
);
export const IconAttach = (p: P) => (
  <svg {...base(p)}>
    <path d="m13 7.5-4.8 4.8a3 3 0 0 1-4.2-4.2l5-5a2 2 0 0 1 2.8 2.8l-5 5a1 1 0 0 1-1.4-1.4L10 4.8" />
  </svg>
);
export const IconLink = (p: P) => (
  <svg {...base(p)}>
    <path d="M6.8 9.2a2.6 2.6 0 0 0 3.7 0l2.2-2.2a2.6 2.6 0 0 0-3.7-3.7l-.7.7M9.2 6.8a2.6 2.6 0 0 0-3.7 0L3.3 9a2.6 2.6 0 0 0 3.7 3.7l.7-.7" />
  </svg>
);
/** A merge or pull request (LAUN-95). */
export const IconMerge = (p: P) => (
  <svg {...base(p)}>
    <circle cx="4.5" cy="3.8" r="1.5" />
    <circle cx="4.5" cy="12.2" r="1.5" />
    <circle cx="11.5" cy="8" r="1.5" />
    <path d="M4.5 5.3v5.4M6 8h4" />
  </svg>
);
/** A recorded field edit (LAUN-95): a pencil. */
export const IconEdit = (p: P) => (
  <svg {...base(p)}>
    <path d="m10.5 3 2.5 2.5L6 12.5l-3 .5.5-3L10.5 3Z" />
  </svg>
);
