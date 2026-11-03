import { useMemo, useState } from 'react';
import type { Attachment } from '../../api/types';
import { apiOrigin } from '../../env';
import { renderedImageSources } from '../../lib/markdown';
import { Dialog } from '../../ui/Dialog';
import detail from './detail.module.css';

/** The kind of preview a file attachment gets (LAUN-105). Links and other files get none. */
export function previewKind(a: Attachment): 'image' | 'video' | null {
  if (a.kind !== 'file' || !a.downloadUrl) return null;
  if (a.contentType === 'image/png' || a.contentType === 'image/jpeg' || a.contentType === 'image/gif')
    return 'image';
  // mp4 plays inline since LAUN-107: the CSP has media-src for the files host (web/deploy/security.inc.template).
  if (a.contentType === 'video/mp4') return 'video';
  return null;
}

/**
 * One inline preview (an image, or an mp4: a thumbnail that opens a player dialog, LAUN-159). The image is the API `downloadUrl` (never a silo URL); a click or Enter shows it larger
 * in a dialog over the page (LAUN-117; no new tab). The frame has a FIXED height (the API sends no dimensions), the image is contained in it
 * and loads lazily, so nothing shifts when it arrives.
 */
export function AttachmentPreview({ a }: { a: Attachment }) {
  const [open, setOpen] = useState(false);
  const kind = previewKind(a);
  if (!kind || !a.downloadUrl) return null;
  const name = a.filename || a.title || 'attachment';
  if (kind === 'video') {
    return (
      <figure className={detail.preview}>
        <button
          type="button"
          className={`${detail.previewLink} ${detail.playLink}`}
          onClick={() => setOpen(true)}
          aria-label={`Play ${name}`}
          aria-haspopup="dialog"
        >
          {/* A muted first frame, no controls: the button opens the player (LAUN-159). Never autoplays. */}
          <video
            className={detail.previewImg}
            src={a.downloadUrl}
            preload="metadata"
            muted
            playsInline
            tabIndex={-1}
            aria-hidden="true"
          />
          <span className={detail.playBadge} aria-hidden="true">
            &#9654;
          </span>
        </button>
        {open && (
          <Dialog title={name} width={1100} onClose={() => setOpen(false)}>
            <video
              className={detail.lightboxVideo}
              src={a.downloadUrl}
              controls
              playsInline
              preload="metadata"
              aria-label={`Video: ${name}`}
            />
            <p className={detail.lightboxOpen}>
              <a className={detail.downloadBtn} href={a.downloadUrl} download={name}>
                Download {name}
              </a>
            </p>
          </Dialog>
        )}
      </figure>
    );
  }
  return (
    <figure className={detail.preview}>
      <button
        type="button"
        className={detail.previewLink}
        onClick={() => setOpen(true)}
        aria-label={`View ${name} larger`}
        aria-haspopup="dialog"
      >
        <img className={detail.previewImg} src={a.downloadUrl} alt={name} loading="lazy" decoding="async" />
      </button>
      {open && (
        <Dialog title={name} width={1100} onClose={() => setOpen(false)}>
          <img className={detail.lightboxImg} src={a.downloadUrl} alt={name} decoding="async" />
          <p className={detail.lightboxOpen}>
            <a href={a.downloadUrl} target="_blank" rel="noreferrer noopener">
              Open original in a new tab
            </a>
          </p>
        </Dialog>
      )}
    </figure>
  );
}

/**
 * True when the description REALLY renders this attachment (LAUN-115): an image file, and an `<img>` that the
 * Markdown component renders (`renderedImageSources`: same parse, so code, escapes and blocked URLs give none)
 * whose URL path ends in `/attachments/<id>/download`. A video is never shown by markdown, so it is exempt.
 */
export function shownInText(a: Attachment, sources: string[]): boolean {
  if (previewKind(a) !== 'image') return false;
  const suffix = `/attachments/${a.id}/download`.toLowerCase();
  return sources.some((src) => {
    try {
      return new URL(src, apiOrigin() + '/').pathname.toLowerCase().endsWith(suffix);
    } catch {
      return false;
    }
  });
}

/**
 * The issue's attachments area: image previews, then a chip per other file. Nothing when none. A previewable file
 * that the description already shows (LAUN-115) is not previewed again: it is a chip, so the row still lists it.
 */
export function AttachmentsArea({
  items,
  description,
}: {
  items: Attachment[] | undefined;
  description?: string | null;
}) {
  const sources = useMemo(() => renderedImageSources(description ?? ''), [description]);
  const files = (items ?? []).filter((a) => a.kind === 'file' && a.downloadUrl);
  if (!files.length) return null;
  const shown = files.filter((a) => previewKind(a) && !shownInText(a, sources));
  const others = files.filter((a) => !shown.includes(a));
  return (
    <section aria-labelledby="att-h" className={detail.attArea}>
      <h2 id="att-h" className={detail.sectionTitle}>
        Attachments
      </h2>
      {shown.length > 0 && (
        <div className={detail.previewGrid}>
          {shown.map((a) => (
            <AttachmentPreview key={a.id} a={a} />
          ))}
        </div>
      )}
      {others.length > 0 && (
        <ul className={detail.fileChips}>
          {others.map((a) => (
            <li key={a.id}>
              <a className={detail.fileChip} href={a.downloadUrl!} target="_blank" rel="noreferrer noopener">
                {a.filename || a.title}
              </a>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
