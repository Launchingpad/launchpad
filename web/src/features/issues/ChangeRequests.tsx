import { Fragment, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import type { ChangeRequest } from '../../api/types';
import { useIsOrgAdmin } from '../settings/access';
import { Avatar } from '../../ui/Avatar';
import { IconMerge } from '../../ui/Icons';
import { CHECKS_TEXT, providerLabel, repoWebUrl, requestRef, safeHttpUrl, STATE_TEXT } from './changeText';
import detail from './detail.module.css';

function Row({ cr, settingsHref }: { cr: ChangeRequest; settingsHref: string | null }) {
  const ref = requestRef(cr.provider, cr.number);
  // title, author and repoPath come from the git host: plain text nodes, never HTML or Markdown.
  const text = cr.title ? `${ref} ${cr.title}` : ref;
  const href = safeHttpUrl(cr.url);
  // LAUN-199: when a member linked this login, show the member (a claim, display only). The host login stays
  // in the tooltip and the screen reader text, so the link is never the only thing said.
  const who = cr.authorUser;
  const host = providerLabel(cr.provider);
  // LAUN-239: the repository links to its page on the host (cut from the request URL, http(s) only, and only
  // when that path ends in the label shown); the host name links to its connection in Settings for an org
  // admin. Anyone else, or an unsafe URL: plain text.
  const repoHref = repoWebUrl(cr.provider, cr.url, cr.number, cr.repoPath);
  const meta: ReactNode[] = [];
  if (cr.repoPath)
    meta.push(
      repoHref ? (
        <a
          href={repoHref}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={`Repository ${cr.repoPath} on ${host}, opens in a new tab`}
        >
          {cr.repoPath}
        </a>
      ) : (
        cr.repoPath
      ),
    );
  if (!who && cr.author) meta.push(`by ${cr.author}`);
  meta.push(
    settingsHref ? (
      <Link to={settingsHref} aria-label={`${host} integration settings`}>
        {host}
      </Link>
    ) : (
      host
    ),
  );
  return (
    <li className={detail.crRow}>
      <span className={detail.crIcon} aria-hidden="true">
        <IconMerge />
      </span>
      <div className={detail.crBody}>
        {href ? (
          <a className={detail.crTitle} href={href} target="_blank" rel="noreferrer noopener">
            {text}
            <span className="sr-only"> (opens in a new tab)</span>
          </a>
        ) : (
          <span className={detail.crTitle}>{text}</span>
        )}
        <div className={detail.crMeta}>
          <span className={`${detail.crChip} ${detail[`cr_${cr.state}`]}`}>{STATE_TEXT[cr.state]}</span>
          {cr.changesRequested && (
            <span className={`${detail.crChip} ${detail.cr_warn}`}>Changes requested</span>
          )}
          {cr.checks !== null && (
            <span
              className={`${detail.crChip} ${cr.checks === 'failure' ? detail.cr_danger : cr.checks === 'pending' ? detail.cr_warn : ''}`}
            >
              {CHECKS_TEXT[cr.checks]}
            </span>
          )}
          <span className={detail.crText}>
            {who && (
              <>
                <span
                  title={`${cr.author ? `${cr.author} on ${providerLabel(cr.provider)}: ` : ''}linked by the member, not verified`}
                >
                  <Avatar name={who.name} username={who.username} /> by {who.name}
                </span>{' '}
                <span>(linked, not verified)</span>
                <span className="sr-only"> (host login {cr.author || 'unknown'})</span>
                {meta.length > 0 ? ' · ' : ''}
              </>
            )}
            {meta.map((m, i) => (
              <Fragment key={i}>
                {i > 0 ? ' · ' : ''}
                {m}
              </Fragment>
            ))}
          </span>
        </div>
      </div>
    </li>
  );
}

/**
 * The merge and pull requests linked to the issue (LAUN-83 server, LAUN-95 web). Nothing at all, not even the
 * heading, while the issue loads or when it has none (the key is absent). `checks` is null until the checks
 * ticket (LAUN-85) sets it: then no badge.
 */
export function ChangeRequests({ items }: { items: ChangeRequest[] | undefined }) {
  const org = useOrgSlug();
  // Settings > Integrations is admin only (settingsPages): a member gets the host name as text, not a dead link.
  const isAdmin = useIsOrgAdmin();
  if (!items || items.length === 0) return null;
  return (
    <section aria-labelledby="changes-h" className={detail.crBlock}>
      <h2 id="changes-h" className={detail.crHead}>
        Changes
      </h2>
      <ul className={detail.crList}>
        {items.map((cr) => (
          <Row
            key={cr.id}
            cr={cr}
            settingsHref={
              isAdmin && cr.integrationId
                ? `/${org}/settings/integrations/${encodeURIComponent(cr.integrationId)}`
                : null
            }
          />
        ))}
      </ul>
    </section>
  );
}
