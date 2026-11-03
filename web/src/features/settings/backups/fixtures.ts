import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../../test/mockApi';
import { page, sessionRoute } from '../../../test/join';

// LAUN-78: fixtures for the Backups page tests.

export const MARKER = 'RAW-SERVER-WORDS-MARKER';
export const FP = 'ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12';

export function status(over: Record<string, unknown> = {}) {
  return {
    encryptionKeySet: true,
    keyMismatch: false,
    signingPublic: 'c2lnbmluZy1wdWJsaWMta2V5LWJhc2U2NC1leGFtcGxlLTEyMzQ=',
    signingKeyUnreadable: false,
    supportedTypes: ['s3', 'webdav'],
    recipients: [],
    activeRecipients: 1,
    everHadRecipient: true,
    destinations: 1,
    enabledDestinations: 0,
    destinationsUnreadable: 0,
    newestDump: {
      id: 'd1',
      fileName: 'dump.sql',
      status: 'ok',
      startedAt: '2026-10-28T03:00:00Z',
      finishedAt: '2026-10-28T03:01:00Z',
      bytes: 5_000_000,
      migrationVersion: 21,
      fileRemoved: false,
      errorText: '',
    },
    noRecentDump: false,
    mailReady: true,
    staleAlertHours: 48,
    staleDestinations: [],
    failedDestinations: [],
    filesStored: false,
    filesNotBackedUp: [],
    banners: [],
    ...over,
  };
}

export function bsettings(over: Record<string, unknown> = {}) {
  return {
    installId: 'a1b2c3d4',
    signingPublic: 'c2lnbmluZy1wdWJsaWMta2V5LWJhc2U2NC1leGFtcGxlLTEyMzQ=',
    notifyOnFailure: true,
    notifyOnSuccess: false,
    staleAlertHours: 48,
    updatedAt: '2026-10-25T10:00:00.000000Z',
    updatedBy: null,
    ...over,
  };
}

export function recipient(over: Record<string, unknown> = {}) {
  const fp = (over.fingerprint as string) ?? FP;
  return {
    publicKey: 'age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq',
    fingerprint: fp,
    shortFingerprint: fp.slice(0, 12),
    state: 'active',
    addedAt: '2026-10-01T10:00:00Z',
    addedBy: 'u1',
    effectiveAt: '2026-10-01T10:00:00Z',
    removeAt: null,
    removeBy: null,
    ...over,
  };
}

export function run(over: Record<string, unknown> = {}) {
  return {
    id: 'r1',
    destinationId: 'dest1',
    destinationName: 'Offsite S3',
    dumpId: null,
    trigger: 'schedule',
    status: 'succeeded',
    attempt: 1,
    willRetry: false,
    retryAt: null,
    queuedAt: '2026-10-28T04:15:00Z',
    startedAt: '2026-10-28T04:15:05Z',
    finishedAt: '2026-10-28T04:16:00Z',
    bytes: 2_000_000,
    sha256: null,
    remoteName: null,
    steps: [],
    errorCode: '',
    errorText: '',
    ...over,
  };
}

export function destination(over: Record<string, unknown> = {}) {
  return {
    id: 'dest1',
    name: 'Offsite S3',
    type: 's3',
    typeSupported: true,
    enabled: false,
    enabledSince: null,
    config: {
      endpoint: 'https://s3.example.com',
      bucket: 'backups',
      region: 'eu-west-1',
      folder: 'launchpad',
      addressing: 'path',
    },
    secretsSet: ['accessKey', 'secretKey'],
    secretsUnreadable: false,
    schedule: 'daily',
    atUtc: '04:15',
    weekday: null,
    keepLast: 14,
    verifyEveryDays: 7,
    lastRun: null,
    createdAt: '2026-10-01T10:00:00Z',
    updatedAt: '2026-10-01T10:00:00Z',
    updatedBy: null,
    ...over,
  };
}

export interface BackupState {
  status: Record<string, unknown>;
  settings: Record<string, unknown>;
  recipients: Record<string, unknown>[];
  destinations: Record<string, unknown>[];
  runs: Record<string, unknown>[];
  changes: Record<string, unknown>[];
  runById: Record<string, Record<string, unknown>>;
}

export function backupServer(
  opts: {
    platform?: boolean;
    state?: Partial<BackupState>;
    routes?: Record<string, Handler>;
  } = {},
) {
  const st: BackupState = {
    status: status(),
    settings: bsettings(),
    recipients: [recipient()],
    destinations: [destination()],
    runs: [],
    changes: [],
    runById: {},
    ...opts.state,
  };
  const P = '/api/v1/platform/backups';
  let saves = 0;
  const routes: Record<string, Handler> = {
    [`GET ${P}/status`]: () => ({ json: st.status }),
    [`GET ${P}/settings`]: () => ({ json: st.settings }),
    [`PUT ${P}/settings`]: (r: MockRequest) => {
      // A real server stamps a new updatedAt on every save.
      saves++;
      st.settings = {
        ...st.settings,
        ...(r.body as object),
        updatedAt: `2026-10-28T10:00:0${saves}.000000Z`,
      };
      return { json: st.settings };
    },
    [`GET ${P}/recipients`]: () => ({ json: { items: st.recipients, everHadRecipient: true, limit: 5 } }),
    [`GET ${P}/destinations`]: () => ({ json: page(st.destinations) }),
    [`GET ${P}/runs`]: () => ({ json: page(st.runs) }),
    [`GET ${P}/changes`]: () => ({ json: page(st.changes) }),
    [`GET ${P}/runs/:id`]: (r: MockRequest) => ({
      json: st.runById[r.params.id!] ?? run({ id: r.params.id }),
    }),
    ...opts.routes,
  };
  const api = mockApi({
    ...baseRoutes(),
    ...sessionRoute({ platform: opts.platform ?? true }),
    ...routes,
  });
  return { api, st };
}

export const callsTo = (api: { calls: MockRequest[] }, method: string, suffix: string) =>
  api.calls.filter((c) => c.method === method && c.url.pathname.endsWith(suffix));
