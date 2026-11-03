import type { ReactElement } from 'react';
import { render } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { OrgProvider } from '../api/org';
import { ToastProvider } from '../ui/Toast';

export function testClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: Infinity, staleTime: 0 },
      mutations: { retry: false },
    },
  });
}

/** Render inside providers, with the org taken from the URL exactly like the app does. */
export function renderAt(
  ui: ReactElement,
  opts: { path?: string; route?: string; client?: QueryClient } = {},
) {
  const client = opts.client ?? testClient();
  const route = opts.route ?? '/:org/*';
  const result = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[opts.path ?? '/orbit/issues']}>
        <ToastProvider>
          <Routes>
            <Route path={route} element={<OrgFromRoute>{ui}</OrgFromRoute>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...result, client };
}

import { useLocation, useNavigate, useParams } from 'react-router-dom';
import type { ReactNode } from 'react';
function OrgFromRoute({ children }: { children: ReactNode }) {
  const { org = '' } = useParams();
  return <OrgProvider slug={org}>{children}</OrgProvider>;
}

// ---- whole-app rendering ----
import { QueryClientProvider as QCP } from '@tanstack/react-query';
import { App } from '../App';
import { onUnauthorized } from '../api/client';
import { keys } from '../api/keys';

/** The router location of the last `renderApp`, for tests that check the URL. */
export const currentLocation = { pathname: '', search: '', go: (_delta: number) => {} };
function LocationProbe() {
  const l = useLocation();
  const nav = useNavigate();
  currentLocation.go = (d) => nav(d); // history back / forward, as the browser button does
  currentLocation.pathname = l.pathname;
  currentLocation.search = l.search;
  return null;
}

export function renderApp(path: string | string[] = '/', client: QueryClient = testClient()) {
  onUnauthorized(() => {
    if (client.getQueryData(keys.session) !== null) client.setQueryData(keys.session, null);
  });
  const result = render(
    <QCP client={client}>
      <MemoryRouter
        initialEntries={Array.isArray(path) ? path : [path]}
        initialIndex={Array.isArray(path) ? path.length - 1 : 0}
      >
        <LocationProbe />
        <ToastProvider>
          <App />
        </ToastProvider>
      </MemoryRouter>
    </QCP>,
  );
  return { ...result, client };
}
