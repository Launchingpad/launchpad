import type { GitDelivery, GitDeliverySummary } from '../../../api/types';
import { fullDate } from '../../../lib/time';

// LAUN-93: the deliveries ring in words (LAUN-87, decisions-T17 #9). A row has no payload, so every sentence here
// comes from the result and our own error code. Nothing the host sent is shown.

export const RESULT_TEXT: Record<GitDelivery['result'], string> = {
  processed: 'Processed',
  ignored: 'Ignored',
  duplicate: 'Duplicate (already handled)',
  unmapped: 'Repository not mapped',
  bad_signature: 'Refused: wrong secret or signature',
  bad_request: 'Refused: bad request',
  error: 'Error',
};

const CODE_TEXT: Record<string, string> = {
  unauthenticated: 'The secret or signature did not match.',
  invalid_token: 'The secret or signature did not match.',
  bad_request: 'The request was not valid.',
  validation_failed: 'The request was not valid.',
  length_required: 'The request had no length.',
  payload_too_large: 'The request was too large.',
  unsupported_media_type: 'The content type is not supported.',
  overloaded: 'Launchpad was busy.',
  rate_limited: 'Too many requests.',
  not_found: 'No matching connection.',
  forbidden: 'The connection is off.',
  internal_error: 'Launchpad had a problem.',
  internal: 'Launchpad had a problem.',
};

/** Our own error code in plain words. An unknown code gets a fixed sentence; the code itself is not printed. */
export function deliveryCodeText(code: string | null | undefined): string {
  if (!code) return '';
  return CODE_TEXT[code] ?? 'Launchpad refused the request.';
}

export function resultText(result: GitDelivery['result']): string {
  return RESULT_TEXT[result] ?? 'Unknown result';
}

/** The "Last delivery" line: the result and the time, or a fixed sentence when there is none. */
export function lastDeliveryText(d: GitDeliverySummary | null | undefined): string {
  if (!d) return 'No deliveries yet';
  const when = fullDate(d.receivedAt);
  return `${resultText(d.result)}${when ? `, ${when}` : ''}`;
}
