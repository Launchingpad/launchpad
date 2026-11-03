import { baseRoutes, mockApi, type Handler, type MockRequest } from './mockApi';

// shared fixtures for the invite, sign-up and platform admin tests.

export const MARKER = 'RAW-SERVER-TEXT-MARKER';

export function sessionRoute(
  opts: { role?: 'admin' | 'member'; platform?: boolean } = {},
): Record<string, Handler> {
  return {
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'chen@example.com',
          isPlatformAdmin: opts.platform ?? false,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: opts.role ?? 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
  };
}

export function platformSettings(over: Record<string, unknown> = {}) {
  return {
    mailProvider: 'env',
    postmarkFrom: '',
    postmarkMessageStream: '',
    postmarkTokenSet: false,
    smtpHost: '',
    smtpPort: 587,
    smtpSecurity: 'starttls',
    smtpUsername: '',
    smtpPasswordSet: false,
    smtpFromAddress: '',
    smtpFromName: '',
    signupMode: 'invite',
    activeMail: { transport: 'dev-log', source: 'environment' },
    loginAvailable: true,
    encryptionKeySet: true,
    secretsUnreadable: false,
    updatedAt: '2026-10-25T10:00:00.000000Z',
    updatedBy: null,
    ...over,
  };
}

export function invite(id: string, over: Record<string, unknown> = {}) {
  return {
    id,
    email: `${id}@example.com`,
    role: 'member',
    status: 'pending',
    emailed: true,
    createdBy: 'u1',
    createdAt: new Date(Date.now() - 3 * 3600_000).toISOString(),
    expiresAt: '2099-01-01T00:00:00Z',
    acceptedAt: null,
    acceptedBy: null,
    revokedAt: null,
    teamAccess: 'all',
    teams: [],
    ...over,
  };
}

export function signupReq(id: string, over: Record<string, unknown> = {}) {
  return {
    id,
    email: `${id}@example.com`,
    name: `Name ${id}`,
    username: id,
    note: '',
    status: 'pending',
    emailVerified: true,
    emailVerifiedAt: '2026-10-25T10:01:00Z',
    createdAt: new Date(Date.now() - 2 * 86400_000).toISOString(),
    expiresAt: '2099-01-01T00:00:00Z',
    decidedAt: null,
    decidedBy: null,
    userId: null,
    rejectReason: '',
    blocked: false,
    ...over,
  };
}

export const page = <T>(items: T[]) => ({ items, nextCursor: null });

export function joinServer(
  opts: { role?: 'admin' | 'member'; platform?: boolean; routes?: Record<string, Handler> } = {},
) {
  return mockApi({ ...baseRoutes(), ...sessionRoute(opts), ...opts.routes });
}

export const callsTo = (api: { calls: MockRequest[] }, method: string, suffix: string) =>
  api.calls.filter((c) => c.method === method && c.url.pathname.endsWith(suffix));

export const webStorage = () => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });
