import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { challengeWorking, issuerWorking } from "../lib/certificates";
import { request } from "./client";
import { keys } from "./queries";
import type { CertificateAuthority, CertificateChallengeList, CertificateIssuer, CertificateIssuerInput, CertificateZone, RouteCertificate } from "./types";

export interface CertificateUpload {
  name: string;
  certificate_pem: string;
  private_key_pem: string;
}

export const certificatesKey = (workloadId: string) => [...keys.workload(workloadId), "certificates"] as const;

const path = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/certificates`;

// Metadata only: the API never returns certificate chains or private keys.
export function certificatesQuery(workloadId: string) {
  return queryOptions({
    queryKey: certificatesKey(workloadId),
    queryFn: ({ signal }) => request<RouteCertificate[]>("GET", path(workloadId), undefined, signal),
  });
}

// The variables carry the private key, so the mutation keeps no cache time. Callers reset it once they have read the
// outcome, which drops the variables while the form is still mounted.
export function useUploadCertificate(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: CertificateUpload) => request<RouteCertificate>("POST", path(workloadId), input),
    onSuccess: (certificate) => client.setQueryData<RouteCertificate[]>(certificatesKey(workloadId), (list) => [certificate, ...(list ?? []).filter((item) => item.id !== certificate.id)]),
    onSettled: () => client.invalidateQueries({ queryKey: certificatesKey(workloadId), exact: true }),
  });
}

export function useDeleteCertificate(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (certificateId: string) => request<{ state: "deleted" }>("DELETE", `${path(workloadId)}/${encodeURIComponent(certificateId)}`),
    onSuccess: (_, certificateId) => client.setQueryData<RouteCertificate[]>(certificatesKey(workloadId), (list) => list?.filter((item) => item.id !== certificateId)),
    // A conflict means the saved routes or operations changed since they were loaded, so refresh those too.
    onSettled: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: certificatesKey(workloadId), exact: true }),
        client.invalidateQueries({ queryKey: keys.workload(workloadId), exact: true }),
        client.invalidateQueries({ queryKey: keys.operations }),
      ]),
  });
}

export const certificateZonesKey = (workloadId: string) => [...keys.workload(workloadId), "certificate-zones"] as const;
export const certificateIssuersKey = (workloadId: string) => [...keys.workload(workloadId), "certificate-issuers"] as const;
export const certificateAuthorityKey = (workloadId: string) => [...keys.workload(workloadId), "certificate-authority"] as const;
export const certificateChallengesKey = (workloadId: string) => [...keys.workload(workloadId), "certificate-challenges"] as const;
export const zoneCertificateChallengesKey = (zoneId: string) => ["cloudflare", zoneId, "certificate-challenges"] as const;

const workloadPath = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}`;
const issuerPath = (workloadId: string, issuerId: string) => `${workloadPath(workloadId)}/certificate-issuers/${encodeURIComponent(issuerId)}`;

// The team's Cloudflare zone shares as this resource may use them. Never includes tokens or credentials.
export function certificateZonesQuery(workloadId: string) {
  return queryOptions({
    queryKey: certificateZonesKey(workloadId),
    queryFn: ({ signal }) => request<CertificateZone[]>("GET", `${workloadPath(workloadId)}/certificate-zones`, undefined, signal),
  });
}

// Polls while a request, reconciliation or route update is in progress so the phase and latest version stay current.
export function certificateIssuersQuery(workloadId: string) {
  return queryOptions({
    queryKey: certificateIssuersKey(workloadId),
    queryFn: ({ signal }) => request<CertificateIssuer[]>("GET", `${workloadPath(workloadId)}/certificate-issuers`, undefined, signal),
    refetchInterval: (query) => (query.state.data?.some((issuer) => issuerWorking(issuer.phase)) ? 5_000 : 60_000),
  });
}

// The panel operator's ACME authority and its current terms. Reading it makes the panel contact that authority, which may
// be a public one, so callers enable it only once someone chooses ACME DNS-01. It is refreshed explicitly (or after the
// panel reports changed terms) rather than on focus, and a failure is shown with Retry instead of retried silently.
export function certificateAuthorityQuery(workloadId: string) {
  return queryOptions({
    queryKey: certificateAuthorityKey(workloadId),
    queryFn: ({ signal }) => request<CertificateAuthority>("GET", `${workloadPath(workloadId)}/certificate-authority`, undefined, signal),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
    retry: false,
  });
}

const challengeInterval = (data: CertificateChallengeList | undefined) => (data?.items.some((item) => challengeWorking(item.phase)) ? 10_000 : 60_000);

// Unresolved DNS-01 challenge records for a resource, including cleanup that outlives its issuers.
export function certificateChallengesQuery(workloadId: string) {
  return queryOptions({
    queryKey: certificateChallengesKey(workloadId),
    queryFn: ({ signal }) => request<CertificateChallengeList>("GET", `${workloadPath(workloadId)}/certificate-challenges`, undefined, signal),
    refetchInterval: (query) => challengeInterval(query.state.data),
  });
}

// Unresolved DNS-01 challenge records in a zone, for its connection owner or an instance administrator. Includes records
// whose resource was deleted.
export function zoneCertificateChallengesQuery(zoneId: string) {
  return queryOptions({
    queryKey: zoneCertificateChallengesKey(zoneId),
    queryFn: ({ signal }) => request<CertificateChallengeList>("GET", `/api/cloudflare/${encodeURIComponent(zoneId)}/certificate-challenges`, undefined, signal),
    refetchInterval: (query) => challengeInterval(query.state.data),
  });
}

// An issued version appears in the certificate list, routes may change after a renewal, and DNS challenges follow the
// issuer's requests and removal, so refresh those with issuers.
function refreshIssuers(client: QueryClient, workloadId: string) {
  return Promise.all([
    client.invalidateQueries({ queryKey: certificateIssuersKey(workloadId), exact: true }),
    client.invalidateQueries({ queryKey: certificateChallengesKey(workloadId), exact: true }),
    client.invalidateQueries({ queryKey: certificatesKey(workloadId), exact: true }),
    client.invalidateQueries({ queryKey: keys.workload(workloadId), exact: true }),
  ]);
}

export function useCreateCertificateIssuer(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: CertificateIssuerInput) => request<{ id: string; phase: string }>("POST", `${workloadPath(workloadId)}/certificate-issuers`, input),
    onSettled: () => refreshIssuers(client, workloadId),
  });
}

export type CertificateIssuerUpdate = { auto_renew: boolean } | { retry: true };

export function useUpdateCertificateIssuer(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, update }: { id: string; update: CertificateIssuerUpdate }) => request<{ ok: true }>("PUT", issuerPath(workloadId, id), update),
    onSettled: () => refreshIssuers(client, workloadId),
  });
}

export function useRemoveCertificateIssuer(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: true }>("DELETE", issuerPath(workloadId, id)),
    onSuccess: (_, id) => client.setQueryData<CertificateIssuer[]>(certificateIssuersKey(workloadId), (list) => list?.filter((item) => item.id !== id)),
    onSettled: () => refreshIssuers(client, workloadId),
  });
}
