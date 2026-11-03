import { useEffect, useId, useRef, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { usePageTitle } from '../../lib/documentTitle';
import { IconChevron } from '../../ui/Icons';
import styles from './settingsParts.module.css';

/**
 * The shared building blocks of every settings page (LAUN-26): a page with a title, sections with a
 * heading, cards, and rows (label and one line of help on the left, the control on the right).
 * No page keeps its own layout.
 */

/** The page. The title takes focus when the page appears, so a screen reader starts there after a route change. */
export function SettingsPage({
  title,
  description,
  wide,
  children,
}: {
  title: string;
  description?: ReactNode;
  /** Pages with wide tables use the wider column. */
  wide?: boolean;
  children: ReactNode;
}) {
  usePageTitle('Settings', title);
  const ref = useRef<HTMLHeadingElement>(null);
  useEffect(() => ref.current?.focus(), []);
  return (
    <div className={`${styles.page} ${wide ? styles.wide : ''}`}>
      <h1 className={styles.title} tabIndex={-1} ref={ref}>
        {title}
      </h1>
      {description && <p className={styles.lead}>{description}</p>}
      {children}
    </div>
  );
}

/** A heading with an optional help line, above one or more cards. */
export function SettingsSection({
  title,
  description,
  id: given,
  children,
}: {
  title?: string;
  /** The id of the heading, for a form inside that names itself by it. */
  id?: string;
  description?: ReactNode;
  children: ReactNode;
}) {
  const made = useId();
  const id = given ?? made;
  return (
    <section className={styles.section} aria-labelledby={title ? id : undefined}>
      {title && (
        <h2 id={id} className={styles.sectionTitle}>
          {title}
        </h2>
      )}
      {description && <p className={styles.sectionHint}>{description}</p>}
      {children}
    </section>
  );
}

/** A rounded card. Rows inside it are split by thin lines. `padded` is for a table or a form instead of rows. */
export function SettingsCard({ padded, children }: { padded?: boolean; children: ReactNode }) {
  return <div className={`${styles.card} ${padded ? styles.padded : ''}`}>{children}</div>;
}

/**
 * One row. With `to` the whole row is a link to a sub page and ends in a chevron; otherwise `children` is
 * the control. `labelFor` ties the label to a control by id: a row whose control is an input, select or
 * textarea MUST pass it (the label is a plain span without it). A test in settingsArea.test.tsx checks this.
 */
export function SettingsRow({
  label,
  description,
  labelFor,
  to,
  children,
}: {
  label: string;
  description?: ReactNode;
  labelFor?: string;
  to?: string;
  children?: ReactNode;
}) {
  const text = (
    <div className={styles.rowText}>
      {labelFor ? (
        <label className={styles.rowLabel} htmlFor={labelFor}>
          {label}
        </label>
      ) : (
        <span className={styles.rowLabel}>{label}</span>
      )}
      {description && <span className={styles.rowHint}>{description}</span>}
    </div>
  );
  if (to) {
    return (
      <Link to={to} className={`${styles.row} ${styles.rowLink}`}>
        {text}
        <IconChevron className={styles.go} />
      </Link>
    );
  }
  return (
    <div className={styles.row}>
      {text}
      {children !== undefined && <div className={styles.control}>{children}</div>}
    </div>
  );
}

/** The toggle control of a row: a switch button in the accent colour. */
export function SettingsToggle({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      className={`${styles.toggle} ${checked ? styles.on : ''}`}
      onClick={() => onChange(!checked)}
    >
      <span className={styles.knob} aria-hidden="true" />
    </button>
  );
}

/** A notice in place of a page, for someone who may not see it. */
export function NoAccess({ title, hint }: { title: string; hint: string }) {
  return (
    <SettingsPage title={title}>
      <p className={styles.sectionHint}>{hint}</p>
    </SettingsPage>
  );
}
