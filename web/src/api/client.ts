import createClient from 'openapi-fetch';
import type { paths } from './schema';
import { apiBase } from '../env';
import { CsrfMissingError, SessionChangedError, secureFetch } from './http';
import { ApiError, networkError, toApiError } from './errors';

export function createApiClient(
  fetchImpl: (r: Request) => Promise<Response> = secureFetch,
  baseUrl = apiBase(),
) {
  return createClient<paths>({ baseUrl, fetch: fetchImpl });
}

export const client = createApiClient();

type Result<T> = { data?: T; error?: unknown; response: Response };

let unauthorizedHandler: (() => void) | undefined;
export function onUnauthorized(fn: (() => void) | undefined): void {
  unauthorizedHandler = fn;
}

/** Turn an openapi-fetch result into data, or throw an ApiError with a safe message. */
export async function unwrap<T>(p: Promise<Result<T>>): Promise<T> {
  let r: Result<T>;
  try {
    r = await p;
  } catch (e) {
    if (e instanceof CsrfMissingError || e instanceof SessionChangedError) throw e;
    throw networkError();
  }
  if (!r.response.ok) {
    const err = toApiError(r.response, r.error);
    if (err.status === 401) unauthorizedHandler?.();
    throw err;
  }
  return r.data as T;
}

export { ApiError };
