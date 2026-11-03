import type { BackupDestination, BackupDestinationType } from '../../../api/types';

// LAUN-78. The form fields of each destination type. There is no driver catalogue in the API, so this table mirrors
// the config of the drivers. All four types are built (s3, webdav, sftp, dropbox); the field and secret names were
// checked against the merged config.go files (LAUN-146). The type picker offers a type only when the status route
// lists it in `supportedTypes`.

export interface FieldDef {
  name: string;
  label: string;
  hint?: string;
  required?: boolean;
  number?: boolean;
  options?: { value: string; label: string }[];
  /** The value a new form starts with. */
  initial?: string;
}

export interface SecretDef {
  name: string;
  label: string;
  hint?: string;
  multiline?: boolean;
}

export interface TypeDef {
  type: BackupDestinationType;
  label: string;
  summary: string;
  config: FieldDef[];
  secrets: SecretDef[];
  /** Where the trust anchor lives in `config`. */
  anchor: 'tlsPin' | 'hostKey';
  anchorLabel: string;
  /** Secret names that must be given on create (`oneOf`: at least one of the group). */
  requiredSecrets: string[];
  oneOfSecrets?: string[];
  /** A saved destination can be tested; an unsaved form cannot (Dropbox: the code is single use). */
  unsavedTest: boolean;
}

export const TYPES: Record<BackupDestinationType, TypeDef> = {
  s3: {
    type: 's3',
    label: 'S3-compatible storage',
    summary: 'Amazon S3, Backblaze B2, Wasabi, MinIO and other services that speak the S3 API.',
    config: [
      {
        name: 'endpoint',
        label: 'Endpoint',
        required: true,
        hint: 'For example https://s3.eu-west-1.amazonaws.com. No path.',
      },
      { name: 'bucket', label: 'Bucket', required: true },
      {
        name: 'region',
        label: 'Region',
        initial: 'us-east-1',
        hint: 'Leave as it is if the service ignores regions.',
      },
      {
        name: 'folder',
        label: 'Folder',
        hint: 'Optional. Backups go under this folder in the bucket. No slash at the start or the end.',
      },
      {
        name: 'addressing',
        label: 'Addressing',
        initial: 'path',
        options: [
          { value: 'path', label: 'Path style (host/bucket/key)' },
          { value: 'virtual', label: 'Virtual-hosted (bucket.host/key)' },
        ],
      },
    ],
    secrets: [
      { name: 'accessKey', label: 'Access key' },
      { name: 'secretKey', label: 'Secret key' },
    ],
    anchor: 'tlsPin',
    anchorLabel: 'TLS pin',
    requiredSecrets: ['accessKey', 'secretKey'],
    unsavedTest: true,
  },
  webdav: {
    type: 'webdav',
    label: 'WebDAV or Nextcloud',
    summary: 'A WebDAV folder over HTTPS, such as Nextcloud or ownCloud. Use an app password.',
    config: [
      {
        name: 'url',
        label: 'WebDAV base URL',
        required: true,
        hint: 'HTTPS only. Include the full WebDAV path, for example https://cloud.example.com/remote.php/dav/files/<user>/. With only the host name, the base path is wrong and the test fails at sign-in.',
      },
      { name: 'username', label: 'Username', required: true },
      {
        name: 'folder',
        label: 'Folder',
        hint: 'Optional. Backups go in this folder under the base URL. No slash at the start or the end.',
      },
    ],
    secrets: [{ name: 'password', label: 'Password (app password)' }],
    anchor: 'tlsPin',
    anchorLabel: 'TLS pin',
    requiredSecrets: ['password'],
    unsavedTest: true,
  },
  sftp: {
    type: 'sftp',
    label: 'SFTP',
    summary: 'A server you reach over SSH. Use an account that is limited to one folder.',
    config: [
      { name: 'host', label: 'Host', required: true },
      { name: 'port', label: 'Port', number: true, initial: '22' },
      { name: 'username', label: 'Username', required: true },
      {
        name: 'folder',
        label: 'Folder',
        hint: 'Optional. Relative to the login directory. One slash at the start means an absolute path.',
      },
    ],
    secrets: [
      { name: 'password', label: 'Password' },
      {
        name: 'privateKey',
        label: 'Private key',
        multiline: true,
        hint: 'Enter a password or a private key.',
      },
    ],
    anchor: 'hostKey',
    anchorLabel: 'SSH host key',
    requiredSecrets: [],
    oneOfSecrets: ['password', 'privateKey'],
    unsavedTest: true,
  },
  dropbox: {
    type: 'dropbox',
    label: 'Dropbox',
    summary: 'A Dropbox app with "App folder" access. Launchpad sees only that folder.',
    config: [
      {
        name: 'access',
        label: 'App access',
        initial: 'app_folder',
        options: [
          { value: 'app_folder', label: 'App folder (the app sees only its own folder)' },
          { value: 'full_dropbox', label: 'Full Dropbox (needs a folder)' },
        ],
        hint: 'Choose what you set when you made the Dropbox app. App folder is safer.',
      },
      {
        name: 'folder',
        label: 'Folder',
        hint: 'Optional with App folder. Required with Full Dropbox. No slash at the start or the end.',
      },
    ],
    secrets: [
      { name: 'appKey', label: 'App key' },
      { name: 'appSecret', label: 'App secret' },
    ],
    anchor: 'tlsPin',
    anchorLabel: 'TLS pin',
    requiredSecrets: ['appKey', 'appSecret'],
    unsavedTest: false,
  },
};

export const ALL_TYPES = Object.keys(TYPES) as BackupDestinationType[];

export type FormConfig = Record<string, string>;

/** A saved field, or the default of the type when the saved config leaves it out. */
function savedValue(f: FieldDef, saved?: Record<string, unknown>): string {
  const v = saved?.[f.name];
  return v === undefined || v === null || String(v).trim() === '' ? (f.initial ?? '') : String(v).trim();
}

/** The form values of a saved destination (or the defaults of a new one). */
export function configToForm(
  def: TypeDef,
  saved?: Record<string, unknown>,
): { values: FormConfig; pin: string } {
  const values: FormConfig = {};
  for (const f of def.config) {
    values[f.name] = savedValue(f, saved);
  }
  const p = saved?.[def.anchor];
  return { values, pin: typeof p === 'string' ? p : '' };
}

/**
 * The config to send. A saved destination's unknown keys stay (the PATCH sends the whole config). An empty
 * optional field is left out. A number field is sent as a number.
 */
export function buildConfig(
  def: TypeDef,
  values: FormConfig,
  pin: string,
  base?: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...(base ?? {}) };
  for (const f of def.config) {
    const v = (values[f.name] ?? '').trim();
    if (v === '') delete out[f.name];
    else out[f.name] = f.number ? Number(v) : v;
  }
  const p = pin.trim();
  if (p === '') delete out[def.anchor];
  else out[def.anchor] = p;
  return out;
}

function norm(v: unknown): string {
  return v === undefined || v === null ? '' : String(v).trim();
}

/** Whether the connection settings differ from the saved ones (the rule of decisions #25 applies then). */
export function configChanged(
  def: TypeDef,
  saved: BackupDestination,
  values: FormConfig,
  pin: string,
): boolean {
  for (const f of def.config) {
    if (savedValue(f, saved.config) !== (values[f.name] ?? '').trim()) return true;
  }
  return norm(saved.config[def.anchor]) !== pin.trim();
}

/** The check of one field. Returns a fixed sentence, or null. */
export function configProblem(def: TypeDef, values: FormConfig): string | null {
  for (const f of def.config) {
    const v = (values[f.name] ?? '').trim();
    if (f.required && v === '') return `${f.label} is required.`;
    if (f.number && v !== '' && (!/^\d{1,5}$/.test(v) || Number(v) < 1 || Number(v) > 65535))
      return `${f.label} must be a whole number from 1 to 65535.`;
  }
  const folder = (values.folder ?? '').trim();
  if (folder !== '') {
    const lead = def.type === 'sftp' ? /^\/\// : /^\//;
    if (lead.test(folder) || folder.endsWith('/'))
      return def.type === 'sftp'
        ? 'Folder: use at most one slash at the start, and no slash at the end.'
        : 'Folder: no slash at the start or at the end.';
  }
  if (def.type === 'dropbox' && values.access === 'full_dropbox' && folder === '')
    return 'Folder is required with Full Dropbox access.';
  const url = (values.url ?? '').trim();
  if (def.type === 'webdav' && url !== '' && !/^https:\/\//i.test(url))
    return 'The WebDAV base URL must start with https://. A password is never sent over plain http.';
  return null;
}

export interface Schedule {
  schedule: 'daily' | 'weekly';
  atUtc: string;
  weekday: number;
  keepLast: string;
  verifyEveryDays: string;
}

export const NAME_MAX = 60;

/** The check of the schedule and retention fields. */
export function scheduleProblem(s: Schedule): string | null {
  if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(s.atUtc.trim()))
    return 'Enter the time as HH:MM, 24 hour clock, UTC.';
  if (s.schedule === 'weekly' && !(Number.isInteger(s.weekday) && s.weekday >= 0 && s.weekday <= 6))
    return 'Choose a day of the week.';
  const keep = s.keepLast.trim();
  if (!/^\d{1,3}$/.test(keep) || Number(keep) < 1 || Number(keep) > 365)
    return 'Keep must be a whole number from 1 to 365.';
  const verify = s.verifyEveryDays.trim();
  if (!/^\d{1,2}$/.test(verify) || Number(verify) > 90)
    return 'Verify every must be a whole number from 0 to 90.';
  return null;
}
