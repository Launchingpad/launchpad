import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactElement } from 'react';
import { createRoutesFromChildren, matchRoutes } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { App } from './App';

// LAUN-94: the web links in a notification mail must be routes of this app.
// The server test TestSender_MailLinksMatchWebRoutes matches a sent mail against
// the same file; this test matches the file against the route tree of App.tsx.
// vitest runs from web/, so the path is relative to it.
const file = resolve(process.cwd(), '../server/internal/notify/testdata/web_routes.txt');

describe('notification mail links', () => {
  const routes = readFileSync(file, 'utf8')
    .split('\n')
    .map((l) => l.trim().split(/\s+/))
    .filter((f): f is [string, string] => f.length === 2 && !f[0]?.startsWith('#'));
  const tree = createRoutesFromChildren((App() as ReactElement<{ children: ReactElement }>).props.children);

  it('lists the three mail routes', () => {
    expect(routes.map((r) => r[0]).sort()).toEqual(['issue', 'settings', 'unsubscribe']);
  });

  const leafOf = (path: string) => matchRoutes(tree, path)?.at(-1)?.route;

  it.each(routes)('%s: %s is a real route, not the not-found page', (_name, pattern) => {
    const leaf = leafOf(pattern.replace(/:(\w+)/g, 'sample-$1'));
    expect(leaf).toBeDefined();
    expect(leaf?.path).not.toBe('*');
    expect(leaf?.index).not.toBe(true);
  });

  it('the old plural issue path is not a route', () => {
    expect(leafOf('/sample-org/issues/LAUN-1')?.path).not.toBe('issue/:identifier');
  });
});
