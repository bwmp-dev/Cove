import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { RouteCertificate } from "./types";

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
