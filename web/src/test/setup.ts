import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup, configure } from '@testing-library/react';
import { vi } from 'vitest';
import { FakeEventSource } from './fakes';
import { resetBackNavMemory } from '../lib/backNav';
import { setBearerForTests, setMemoryCsrf } from '../api/http';
import { resetThemeWrites } from '../api/queries';
import { resetThemeMemory } from '../lib/theme';

// LAUN-112: findBy / waitFor wait up to 1000 ms by default. A test that renders the whole app needs several
// requests in a row, and on a loaded shared runner (or four `npm test` at once) that took 1.2 to 1.7 s. A
// passing wait returns at once, so a larger limit costs nothing; only a real failure waits the full time.
configure({ asyncUtilTimeout: 5000 });

afterEach(() => {
  cleanup();
  try {
    localStorage.clear(); // LAUN-109: a remembered view must not leak into the next test
  } catch {
    /* a test that stubs storage */
  }
  // LAUN-235: the theme and its variant are attributes on <html>; a test must not leave one for the next.
  document.documentElement.removeAttribute('data-look');
  document.documentElement.removeAttribute('data-theme');
  resetThemeWrites();
  resetThemeMemory();
  resetBackNavMemory(); // the per-entry memory is module state, and every test's first entry has the key `default`
  vi.unstubAllGlobals();
  vi.useRealTimers();
  FakeEventSource.instances = [];
  setBearerForTests(undefined);
  setMemoryCsrf(undefined);
  document.cookie = 'odt_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/';
});

if (!window.matchMedia) {
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;
}
// jsdom has no layout: these are no-ops it lacks.
Element.prototype.scrollIntoView = function () {};
class IO {
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() {
    return [];
  }
}
(globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = IO;

(globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
setBearerForTests(undefined);
