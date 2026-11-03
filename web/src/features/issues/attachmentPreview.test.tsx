import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Attachment } from '../../api/types';
import { AttachmentPreview, AttachmentsArea, previewKind } from './AttachmentPreview';

const att = (over: Partial<Attachment>): Attachment => ({
  id: 'a1',
  issueId: 'i1',
  kind: 'file',
  title: 'shot.png',
  subtitle: '',
  url: null,
  filename: 'shot.png',
  contentType: 'image/png',
  sizeBytes: 10,
  downloadUrl: '/api/v1/orgs/orbit/attachments/a1/download',
  creator: null,
  createdAt: '2026-10-27T00:00:00Z',
  ...over,
});

describe('attachment previews (LAUN-105)', () => {
  it('previewKind: images only, files only', () => {
    expect(previewKind(att({}))).toBe('image');
    expect(previewKind(att({ contentType: 'image/jpeg' }))).toBe('image');
    expect(previewKind(att({ contentType: 'image/gif' }))).toBe('image');
    expect(previewKind(att({ contentType: 'video/mp4' }))).toBe('video');
    expect(previewKind(att({ contentType: 'video/webm' as Attachment['contentType'] }))).toBeNull();
    expect(previewKind(att({ contentType: 'video/quicktime' as Attachment['contentType'] }))).toBeNull();
    expect(previewKind(att({ contentType: 'application/pdf' }))).toBeNull();
    expect(previewKind(att({ kind: 'link', url: 'https://x.test', downloadUrl: null }))).toBeNull();
  });

  it('an image shows inline from the download URL, with alt text and a button to view it larger', () => {
    render(<AttachmentPreview a={att({})} />);
    const img = screen.getByRole('img', { name: 'shot.png' });
    expect(img).toHaveAttribute('src', '/api/v1/orgs/orbit/attachments/a1/download');
    expect(img).toHaveAttribute('loading', 'lazy');
    expect(screen.getByRole('button', { name: 'View shot.png larger' })).toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });

  it('an mp4 is a thumbnail button; it opens a player dialog with a download link, Escape closes and focus returns (LAUN-159)', async () => {
    const { container } = render(
      <AttachmentPreview a={att({ contentType: 'video/mp4', filename: 'demo.mp4', title: 'demo.mp4' })} />,
    );
    const thumb = container.querySelector('video')!;
    expect(thumb).not.toHaveAttribute('controls');
    expect(thumb).not.toHaveAttribute('autoplay');
    expect(thumb).toHaveAttribute('preload', 'metadata');
    expect(thumb.muted).toBe(true);
    const btn = screen.getByRole('button', { name: 'Play demo.mp4' });
    await userEvent.click(btn);
    const dlg = screen.getByRole('dialog');
    const v = dlg.querySelector('video')!;
    expect(v).toHaveAttribute('src', '/api/v1/orgs/orbit/attachments/a1/download');
    expect(v).toHaveAttribute('controls');
    expect(v).toHaveAttribute('preload', 'metadata');
    expect(v.playsInline).toBe(true);
    expect(v).not.toHaveAttribute('autoplay');
    const dl = screen.getByRole('link', { name: 'Download demo.mp4' });
    expect(dl).toHaveAttribute('href', '/api/v1/orgs/orbit/attachments/a1/download');
    expect(dl).toHaveAttribute('download', 'demo.mp4');
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(btn).toHaveFocus();
  });

  it('the attachments area previews images and lists other files as chips; empty shows nothing', () => {
    const { container, rerender } = render(<AttachmentsArea items={[]} />);
    expect(container).toBeEmptyDOMElement();
    rerender(
      <AttachmentsArea
        items={[
          att({}),
          att({ id: 'a2', contentType: 'application/pdf', filename: 'spec.pdf', title: 'spec.pdf' }),
          att({ id: 'a3', kind: 'link', url: 'https://x.test', downloadUrl: null, filename: null }),
        ]}
      />,
    );
    const area = screen.getByRole('region', { name: 'Attachments' });
    expect(within(area).getAllByRole('img')).toHaveLength(1);
    expect(within(area).getByRole('link', { name: 'spec.pdf' })).toBeInTheDocument();
    expect(within(area).queryByText('https://x.test')).not.toBeInTheDocument();
  });

  it('an image the description already shows is a chip, not a second preview (LAUN-115)', () => {
    const desc = '![shot](/api/v1/orgs/orbit/attachments/a1/download)';
    const items = [
      att({}),
      att({ id: 'a2', filename: 'other.png', downloadUrl: '/api/v1/orgs/orbit/attachments/a2/download' }),
    ];
    render(<AttachmentsArea items={items} description={desc} />);
    const area = screen.getByRole('region', { name: 'Attachments' });
    expect(within(area).getAllByRole('img')).toHaveLength(1);
    expect(within(area).getByRole('img', { name: 'other.png' })).toBeInTheDocument();
    expect(within(area).getByRole('link', { name: 'shot.png' })).toBeInTheDocument();
  });

  it('an image that is only an attachment still previews once (LAUN-115)', () => {
    render(<AttachmentsArea items={[att({})]} description="Some text, no image." />);
    expect(within(screen.getByRole('region', { name: 'Attachments' })).getAllByRole('img')).toHaveLength(1);
  });

  const area = () => screen.getByRole('region', { name: 'Attachments' });
  const dl = (id: string) => `/api/v1/orgs/orbit/attachments/${id}/download`;

  it('does not match a longer id, or another attachment URL (LAUN-115)', () => {
    const { unmount } = render(<AttachmentsArea items={[att({})]} description={`![x](${dl('a10')})`} />);
    expect(within(area()).getAllByRole('img')).toHaveLength(1);
    unmount();
    render(<AttachmentsArea items={[att({})]} description={`![x](${dl('zzz')})`} />);
    expect(within(area()).getAllByRole('img')).toHaveLength(1);
  });

  it('a relative path and an own-origin absolute URL both count (LAUN-115)', () => {
    const { unmount } = render(<AttachmentsArea items={[att({})]} description={`![x](${dl('a1')})`} />);
    expect(within(area()).queryByRole('img')).not.toBeInTheDocument();
    unmount();
    render(<AttachmentsArea items={[att({})]} description={`![x](${window.location.origin}${dl('a1')})`} />);
    expect(within(area()).queryByRole('img')).not.toBeInTheDocument();
    expect(within(area()).getByRole('link', { name: 'shot.png' })).toBeInTheDocument();
  });

  it('an off-origin URL is blocked by the renderer, so the preview stays (LAUN-115)', () => {
    render(<AttachmentsArea items={[att({})]} description={`![x](https://evil.test${dl('a1')})`} />);
    expect(within(area()).getAllByRole('img')).toHaveLength(1);
  });

  it('a video named in the description keeps its player in the area (LAUN-115)', () => {
    const v = att({ contentType: 'video/mp4', filename: 'd.mp4', title: 'd.mp4' });
    const { container } = render(<AttachmentsArea items={[v]} description={`![d](${dl('a1')})`} />);
    expect(container.querySelector('video')).not.toBeNull();
  });

  const U = '/api/v1/orgs/orbit/attachments/a1/download';
  const kept = (desc: string) => {
    const { unmount } = render(<AttachmentsArea items={[att({})]} description={desc} />);
    const n = within(screen.getByRole('region', { name: 'Attachments' })).queryAllByRole('img').length;
    unmount();
    return n;
  };

  it('code, escapes and plain links are not rendered images, so the preview stays (LAUN-115)', () => {
    expect(kept('```\n![x](' + U + ')\n```')).toBe(1);
    expect(kept('`![x](' + U + ')`')).toBe(1);
    expect(kept('\\![x](' + U + ')')).toBe(1);
    expect(kept('    ![x](' + U + ')')).toBe(1);
    expect(kept('[x](' + U + ')')).toBe(1);
  });

  it('a title, angle brackets, a query or a hash still render the image, so the preview is hidden (LAUN-115)', () => {
    expect(kept('![x](' + U + ' "title")')).toBe(0);
    expect(kept('![x](<' + U + '>)')).toBe(0);
    expect(kept('![x](' + U + '?a=1#h)')).toBe(0);
    expect(kept('![x](' + U.toUpperCase().replace('/API/', '/api/') + ')')).toBe(0);
  });
});

describe('larger view in a dialog (LAUN-117)', () => {
  const U = '/api/v1/orgs/orbit/attachments/a1/download';
  const open = async () => {
    const user = userEvent.setup();
    render(<AttachmentPreview a={att({})} />);
    const btn = screen.getByRole('button', { name: 'View shot.png larger' });
    await user.click(btn);
    return { user, btn };
  };

  it('a click opens a dialog with the larger image from the download URL, and no new tab', async () => {
    await open();
    const dlg = screen.getByRole('dialog');
    expect(within(dlg).getByRole('img', { name: 'shot.png' })).toHaveAttribute('src', U);
    const orig = screen.getByRole('link', { name: 'Open original in a new tab' });
    expect(orig).toHaveAttribute('href', U);
    expect(orig).toHaveAttribute('target', '_blank');
    expect(orig.getAttribute('rel')).toMatch(/noreferrer/);
    expect(orig.getAttribute('rel')).toMatch(/noopener/);
    expect(within(dlg).getByRole('button', { name: 'Close' })).toBeInTheDocument();
  });

  it('Esc closes it and focus returns to the thumbnail button', async () => {
    const { user, btn } = await open();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(btn).toHaveFocus();
  });

  it('the close button and a click outside close it', async () => {
    const { user } = await open();
    await user.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'View shot.png larger' }));
    fireEvent.mouseDown(screen.getByRole('dialog').parentElement!);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('Enter on the thumbnail opens it', async () => {
    const user = userEvent.setup();
    render(<AttachmentPreview a={att({})} />);
    await user.tab();
    await user.keyboard('{Enter}');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });
});
