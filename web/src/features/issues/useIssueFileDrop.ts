import { useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../../api/errors';
import { keys } from '../../api/keys';
import { useOrgSlug } from '../../api/org';
import { useAttachFiles } from '../../api/queries';
import { ATTACH_OFF, checkFiles } from '../../lib/attachRules';
import { useToast } from '../../ui/Toast';
import { useFileDrop } from '../../ui/useFileDrop';

/** The markdown for an attached file: an image shows, any other file is a link. Always the download URL. */
export function fileMarkdown(file: File, downloadUrl: string): string {
  const name =
    file.name
      .replace(/[[\]\\`]/g, '')
      .replace(/\s+/g, ' ')
      .trim() || 'file';
  return file.type.startsWith('image/') ? `![${name}](${downloadUrl})` : `[${name}](${downloadUrl})`;
}

/**
 * Puts text in a controlled textarea at the caret and tells React (a plain `value =` would be lost:
 * React tracks the value, so the native setter and an `input` event are used).
 */
export function insertAtCaret(el: HTMLTextAreaElement, text: string) {
  const start = el.selectionStart ?? el.value.length;
  const end = el.selectionEnd ?? start;
  const before = el.value.slice(0, start);
  const insert = `${before && !before.endsWith('\n') ? '\n' : ''}${text}\n`;
  const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set;
  set?.call(el, before + insert + el.value.slice(end));
  const caret = before.length + insert.length;
  el.setSelectionRange(caret, caret);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/**
 * Drop files on the issue page (LAUN-58): they upload through the same code as the create dialog's
 * paperclip (`useAttachFiles`) and attach to the issue. A drop that lands on a markdown editor (the
 * description, a comment) also writes the file into the text. Progress and errors go to the toast,
 * which is an aria-live region.
 */
export function useIssueFileDrop(identifier: string | undefined) {
  const org = useOrgSlug();
  const attach = useAttachFiles();
  const toast = useToast();
  const qc = useQueryClient();

  async function upload(files: File[], target: EventTarget) {
    const { ok, refused } = checkFiles(files);
    if (refused) toast.show(refused, { kind: 'error', ms: 9000 });
    if (!ok.length || !identifier) return;
    toast.show(ok.length === 1 ? `Uploading ${ok[0]!.name}…` : `Uploading ${ok.length} files…`);
    const editor =
      target instanceof HTMLTextAreaElement && target.id !== 'issue-title' && !target.readOnly
        ? target
        : null;
    const flags = { off: false };
    const failed = await attach(identifier, ok, {
      onAttached: (f, a) => {
        if (editor?.isConnected && a.downloadUrl) insertAtCaret(editor, fileMarkdown(f, a.downloadUrl));
      },
      onFailed: (_f, err) => {
        if (err instanceof ApiError && err.code === 'attachments_unavailable') flags.off = true;
      },
    });
    void qc.invalidateQueries({ queryKey: keys.issue(org, identifier) });
    if (flags.off) toast.show(ATTACH_OFF, { kind: 'error', ms: 9000 });
    else if (failed.length)
      toast.show(`Could not attach: ${failed.map((f) => f.name).join(', ')}.`, { kind: 'error', ms: 9000 });
    const done = ok.length - failed.length;
    if (done) toast.show(done === 1 ? 'File attached.' : `${done} files attached.`);
  }

  return useFileDrop((files, e) => void upload(files, e.target), !identifier);
}
