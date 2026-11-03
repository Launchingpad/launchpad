import { useId, useState } from 'react';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

/** A secret being typed. `clear` asks the server to delete the stored one. */
export interface SecretEdit {
  value: string;
  clear: boolean;
}
export const NO_EDIT: SecretEdit = { value: '', clear: false };
/** The largest key file the page reads (LAUN-146). A private key is a few KiB; this is far above that. */
export const KEY_FILE_MAX_BYTES = 64 * 1024;
export const KEY_FILE_TOO_BIG = 'This file is too big for a key. A key file is under 64 KiB.';
export const KEY_FILE_FAILED = 'The file could not be read. Try again, or paste the key.';
export const isEntered = (e: SecretEdit | undefined): boolean => !!e && (e.value !== '' || e.clear);

/**
 * A write-only secret field (decisions-T16 #25). The page never has the stored value: it shows only whether one is
 * set. A typed value replaces it on save; Clear deletes it. The box is a password box (a key is a masked textarea).
 */
export function SecretInput({
  label,
  isSet,
  edit,
  onChange,
  disabled,
  multiline,
  hint,
  mustReenter,
}: {
  label: string;
  isSet: boolean;
  edit: SecretEdit;
  onChange: (e: SecretEdit) => void;
  disabled?: boolean;
  multiline?: boolean;
  hint?: string;
  /** The connection changed: the server needs this stored secret again. */
  mustReenter?: boolean;
}) {
  const id = useId();
  let status: string;
  if (edit.value !== '')
    status = isSet ? 'Set. It will be replaced when you save.' : 'Not set. It will be saved.';
  else if (edit.clear) status = 'Set. It will be cleared when you save.';
  else if (isSet && mustReenter) status = 'Set. Enter it again: you changed the connection settings.';
  else status = isSet ? 'Set' : 'Not set';
  const fileId = useId();
  const [fileError, setFileError] = useState<string | null>(null);
  function loadFile(f: File | undefined) {
    setFileError(null);
    if (!f) return;
    if (f.size > KEY_FILE_MAX_BYTES) {
      setFileError(KEY_FILE_TOO_BIG);
      return;
    }
    const r = new FileReader();
    r.onload = () => {
      if (typeof r.result === 'string') onChange({ value: r.result, clear: false });
      else setFileError(KEY_FILE_FAILED);
    };
    r.onerror = () => setFileError(KEY_FILE_FAILED);
    r.readAsText(f);
  }
  const common = {
    id,
    value: edit.value,
    onChange: (e: { target: { value: string } }) => onChange({ value: e.target.value, clear: false }),
    autoComplete: multiline ? 'off' : 'new-password',
    autoCorrect: 'off',
    spellCheck: false,
    disabled,
    placeholder: isSet ? 'Type a new value to replace it' : '',
    'aria-describedby': `${id}-status`,
  };
  return (
    <div className={ui.field}>
      <label htmlFor={id}>{label}</label>
      <div className={styles.secretRow}>
        {multiline ? (
          <textarea
            {...common}
            rows={4}
            className={`${ui.textarea} ${styles.masked}`}
            autoCapitalize="none"
          />
        ) : (
          <input {...common} type="password" className={ui.input} />
        )}
        {isSet && !edit.clear && (
          <button
            type="button"
            className={ui.btn}
            aria-label={`Clear ${label}`}
            disabled={disabled}
            onClick={() => onChange({ value: '', clear: true })}
          >
            Clear
          </button>
        )}
        {edit.clear && (
          <button
            type="button"
            className={ui.btn}
            aria-label={`Keep ${label}`}
            onClick={() => onChange(NO_EDIT)}
          >
            Keep it
          </button>
        )}
      </div>
      <span
        id={`${id}-status`}
        className={mustReenter && isSet && !isEntered(edit) ? styles.warnText : ui.hint}
        data-testid={`secret-status-${label}`}
      >
        {status}
      </span>
      {hint && <span className={ui.hint}>{hint}</span>}
      {multiline && (
        <>
          <span className={ui.hint}>The key shows as plain text in some browsers. Paste it in private.</span>
          <label htmlFor={fileId} className={ui.hint}>
            Or load the key from a file. The page does not show it.
          </label>
          <input
            id={fileId}
            type="file"
            disabled={disabled}
            onChange={(e) => {
              loadFile(e.target.files?.[0]);
              e.target.value = '';
            }}
          />
          {fileError && (
            <span role="alert" className={styles.warnText}>
              {fileError}
            </span>
          )}
        </>
      )}
    </div>
  );
}
