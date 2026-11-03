import type { EventSourceLike } from '../api/events';

/** A stand-in for the browser EventSource. Tests drive it by hand. */
export class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = [];
  readyState = 0;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  closed = false;
  private listeners = new Map<string, ((ev: MessageEvent) => void)[]>();
  constructor(
    public url: string,
    public init?: { withCredentials?: boolean },
  ) {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, fn: (ev: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  open() {
    this.readyState = 1;
    this.onopen?.(new Event('open'));
  }
  emit(type: string, data: unknown) {
    const seq = (data as { seq?: number }).seq;
    for (const fn of this.listeners.get(type) ?? [])
      fn({ data: JSON.stringify(data), lastEventId: String(seq ?? '') } as MessageEvent);
  }
  emitRaw(type: string, data: string) {
    for (const fn of this.listeners.get(type) ?? []) fn({ data, lastEventId: '' } as MessageEvent);
  }
  fail() {
    this.onerror?.(new Event('error'));
  }
  /** The org change-feed streams (`/events/stream`). The shell also opens the inbox stream (LAUN-37). */
  static get eventStreams(): FakeEventSource[] {
    return FakeEventSource.instances.filter((s) => s.url.includes('/events/stream'));
  }
  static get lastEvents(): FakeEventSource | undefined {
    return FakeEventSource.eventStreams[FakeEventSource.eventStreams.length - 1];
  }
  /** The inbox streams (`/notifications/stream`). */
  static get inboxStreams(): FakeEventSource[] {
    return FakeEventSource.instances.filter((s) => s.url.includes('/notifications/stream'));
  }
  static get last(): FakeEventSource | undefined {
    return FakeEventSource.instances[FakeEventSource.instances.length - 1];
  }
}
