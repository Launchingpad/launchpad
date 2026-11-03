import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { client, unwrap } from './client';
import { ApiError } from './errors';
import { keys } from './keys';
import { useOrgSlug } from './org';
import type { CreateProductInput, UpdateProductInput } from './types';

// LAUN-66: the products of Settings, Products (decisions-T18 #24).

/** Marks every products list stale (with and without archived ones) and the teams, which carry `productIds`. */
function refreshProducts(qc: QueryClient, org: string): Promise<unknown> {
  return Promise.all([
    qc.invalidateQueries({ queryKey: keys.products(org) }),
    qc.invalidateQueries({ queryKey: keys.teams(org) }),
  ]);
}

/**
 * One product by slug, an archived one too (LAUN-65). The key sits under `keys.products`, so every product mutation
 * and the `product.*` events refresh it. A 404 is not retried: it is the not-found page.
 */
export function useProduct(slug: string | undefined) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: [...keys.products(org), 'detail', slug ?? ''],
    enabled: !!slug,
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/products/{product}', { params: { path: { org, product: slug! } } }),
      ),
    retry: (n, e) => !(e instanceof ApiError && e.status === 404) && n < 2,
    staleTime: 15_000,
  });
}

/** The whole list (no paging, at most 200). `includeArchived` is part of the key. */
export function useProductList(includeArchived: boolean) {
  const org = useOrgSlug();
  return useQuery({
    queryKey: [...keys.products(org), { includeArchived }],
    queryFn: () =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/products', {
          params: { path: { org }, query: includeArchived ? { includeArchived: true } : {} },
        }),
      ),
    select: (r) => r.items,
    staleTime: 15_000,
  });
}

export function useCreateProduct() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (body: CreateProductInput) =>
      unwrap(client.POST('/api/v1/orgs/{org}/products', { params: { path: { org } }, body })),
    // Also on error (a 409 version_conflict, a 404): the list must show what the server holds, or the next try
    // sends the same stale version. Returning the promise keeps the mutation pending until the refetch is done,
    // so an Archive then an instant Restore uses the new version.
    onSettled: () => refreshProducts(qc, org),
  });
}

export function useUpdateProduct() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ product, body }: { product: string; body: UpdateProductInput }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/products/{product}', { params: { path: { org, product } }, body }),
      ),
    // Also on error (a 409 version_conflict, a 404): the list must show what the server holds, or the next try
    // sends the same stale version. Returning the promise keeps the mutation pending until the refetch is done,
    // so an Archive then an instant Restore uses the new version.
    onSettled: () => refreshProducts(qc, org),
  });
}

export function useDeleteProduct() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: (product: string) =>
      unwrap(client.DELETE('/api/v1/orgs/{org}/products/{product}', { params: { path: { org, product } } })),
    // Also on error (a 409 version_conflict, a 404): the list must show what the server holds, or the next try
    // sends the same stale version. Returning the promise keeps the mutation pending until the refetch is done,
    // so an Archive then an instant Restore uses the new version.
    onSettled: () => refreshProducts(qc, org),
  });
}

/** Adds or removes one team. Both calls are replay-safe on the server (204 either way). */
export function useSetProductTeam() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  return useMutation({
    mutationFn: ({ product, team, member }: { product: string; team: string; member: boolean }) => {
      const params = { params: { path: { org, product, team } } };
      return unwrap(
        member
          ? client.PUT('/api/v1/orgs/{org}/products/{product}/teams/{team}', params)
          : client.DELETE('/api/v1/orgs/{org}/products/{product}/teams/{team}', params),
      );
    },
    onSettled: () => refreshProducts(qc, org),
  });
}
