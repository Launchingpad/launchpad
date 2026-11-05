import { vi } from 'vitest';

/** A minimal XMLHttpRequest for upload tests. Drive it with `respond` and `progress`. */
export class FakeXhr {
  static all: FakeXhr[] = [];
  method = '';
  url = '';
  headers: Record<string, string> = {};
  body: unknown = undefined;
  withCredentials = false;
  status = 0;
  responseText = '';
  aborted = false;
  upload: {
    onprogress: ((e: { lengthComputable: boolean; loaded: number; total: number }) => void) | null;
    onload: (() => void) | null;
  } = { onprogress: null, onload: null };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  private resHeaders: Record<string, string> = {};
  constructor() {
    FakeXhr.all.push(this);
  }
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  setRequestHeader(k: string, v: string) {
    this.headers[k] = v;
  }
  getResponseHeader(k: string) {
    return this.resHeaders[k] ?? null;
  }
  send(body: unknown) {
    this.body = body;
  }
  abort() {
    this.aborted = true;
    this.onabort?.();
  }
  progress(loaded: number, total: number) {
    this.upload.onprogress?.({ lengthComputable: true, loaded, total });
    if (loaded >= total) this.upload.onload?.();
  }
  respond(status: number, json: unknown, headers: Record<string, string> = {}) {
    this.status = status;
    this.responseText = JSON.stringify(json);
    this.resHeaders = headers;
    this.onload?.();
  }
  static install() {
    FakeXhr.all = [];
    vi.stubGlobal('XMLHttpRequest', FakeXhr);
  }
  static last(): FakeXhr {
    return FakeXhr.all[FakeXhr.all.length - 1]!;
  }
}
