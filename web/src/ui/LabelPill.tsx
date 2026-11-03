import type { LabelRef } from '../api/types';
import { IconClose } from './Icons';
import styles from './labelPill.module.css';

/** The colour dot of a label. One look for the pills, the picker rows and the create dialog. */
export function LabelDot({ color }: { color?: string }) {
  return <span className={styles.dot} style={{ background: color }} aria-hidden="true" />;
}

/**
 * The one label pill (LAUN-46): round, thin outline, colour dot, 22px high. Used by issue rows, board
 * cards and the detail page. With `onRemove` it also holds a small "x" button that shows on hover and
 * on focus (always on a touch screen); its name is "Remove label <name>".
 */
export function LabelPill({
  label,
  onRemove,
  onClick,
}: {
  label: Pick<LabelRef, 'name' | 'color'>;
  onRemove?: () => void;
  /** A press on the pill itself (mouse only; the keyboard uses the "+" button). */
  onClick?: () => void;
}) {
  return (
    <span
      className={`${styles.pill} ${onClick ? styles.clickable : ''} ${onRemove ? styles.removable : ''}`}
      onClick={onClick}
    >
      <LabelDot color={label.color} />
      <span className={styles.name} title={label.name}>
        {label.name}
      </span>
      {onRemove && (
        <button
          type="button"
          className={styles.remove}
          data-remove=""
          aria-label={`Remove label ${label.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onRemove();
          }}
        >
          <IconClose width={10} height={10} />
        </button>
      )}
    </span>
  );
}
