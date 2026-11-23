import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { issuerWorking } from "../lib/certificates";
import { request } from "./client";
import { keys } from "./queries";
import type { CertificateIssuer, CertificateIssuerInput, CertificateZone, RouteCertificate } from "./types";

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

// An issued version appears in the certificate list, and routes may change after a renewal, so refresh those with issuers.
function refreshIssuers(client: QueryClient, workloadId: string) {
  return Promise.all([
    client.invalidateQueries({ queryKey: certificateIssuersKey(workloadId), exact: true }),
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
