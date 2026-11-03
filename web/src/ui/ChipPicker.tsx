import { useCallback, useRef, useState, type ReactNode } from 'react';
import { Floating } from './Floating';
import { PickerList, type ListProps } from './Picker';
import styles from '../styles/ui.module.css';

export interface ChipPickerProps extends Omit<ListProps, 'ariaLabel' | 'keyHint'> {
  /** The property name: the accessible name of the chip and of the list. */
  label: string;
  /** What the chip shows now (an icon and a value, or the property name when unset). */
  display: ReactNode;
  /** The value as text, for the chip's accessible name: "Status: Todo". Empty when unset. */
  valueText?: string;
  /** The shortcut letter, shown in the search box. */
  shortcut?: string;
  unset?: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  disabled?: boolean;
  /** Replaces the chip look (LAUN-46: the quiet "+" of the label list). */
  triggerClassName?: string;
  /** The chip's whole accessible name, when it is not `label` (the list keeps `label`). */
  triggerName?: string;
  /** Draws the list itself (LAUN-53: the label list loads usage counts). It gets the final list props. */
  renderList?: (p: ListProps) => ReactNode;
}

/**
 * A compact property chip that opens a searchable list in a floating panel.
 * ARIA: the chip is a button with `aria-haspopup="listbox"`; the panel holds a combobox
 * (the search box) that controls the listbox. Esc closes the panel and puts focus back on the chip.
 */
export function ChipPicker({
  label,
  display,
  valueText,
  shortcut,
  unset,
  open,
  onOpenChange,
  disabled,
  triggerClassName,
  triggerName,
  onSelect,
  multi,
  renderList,
  ...list
}: ChipPickerProps) {
  const [anchor, setAnchor] = useState<HTMLButtonElement | null>(null);
  const trigger = useRef<HTMLButtonElement | null>(null);
  // A stable ref callback: an inline one is called with null and then the element on every render.
  const setTrigger = useCallback((el: HTMLButtonElement | null) => {
    trigger.current = el;
    setAnchor(el);
  }, []);
  // Focus first, then close: the search box is about to leave the page, and focus must not drop to the body.
  const close = (restore: boolean) => {
    if (restore) trigger.current?.focus();
    onOpenChange(false);
  };

  return (
    <>
      <button
        ref={setTrigger}
        type="button"
        className={triggerClassName ?? `${styles.chipBtn} ${unset ? styles.chipUnset : ''}`}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={triggerName ?? (valueText ? `${label}: ${valueText}` : label)}
        title={triggerName ?? (shortcut ? `${label} (${shortcut})` : label)}
        disabled={disabled}
        // While the panel is open a press must not move focus to the dialog node (Safari and Firefox on Mac
        // do not focus a button on press), and the click that closes the panel puts focus on the chip first.
        onMouseDown={(e) => {
          if (open) e.preventDefault();
        }}
        onClick={() => (open ? close(true) : onOpenChange(true))}
        onKeyDown={(e) => {
          // Focus can sit on the chip while its panel is open (a click on the chip). Esc then closes the panel only.
          if (open && e.key === 'Escape') {
            e.stopPropagation();
            e.preventDefault();
            close(true);
          }
        }}
      >
        {display}
      </button>
      {open && anchor && (
        <Floating anchor={anchor} label={label} onClose={close}>
          {(renderList ?? ((p: ListProps) => <PickerList {...p} />))({
            ...list,
            ariaLabel: label,
            keyHint: shortcut,
            multi,
            onSelect: (o) => {
              onSelect(o);
              if (!multi) close(true);
            },
          })}
        </Floating>
      )}
    </>
  );
}
