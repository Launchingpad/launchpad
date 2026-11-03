import { createContext, useContext, type ReactNode } from 'react';

const OrgContext = createContext<string | null>(null);

/** The org slug comes from the route (`/:org/...`) and nowhere else. */
export function OrgProvider({ slug, children }: { slug: string; children: ReactNode }) {
  return <OrgContext.Provider value={slug}>{children}</OrgContext.Provider>;
}

export function useOrgSlug(): string {
  const v = useContext(OrgContext);
  if (!v) throw new Error('useOrgSlug used outside an org route');
  return v;
}
