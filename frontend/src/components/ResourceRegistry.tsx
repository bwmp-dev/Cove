import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import { connectionsQuery } from "../api/connections";
import { inputClass } from "../lib/styles";
import { useOnline } from "../lib/hooks";
import { Button, Field, Notice, QueryError, Section } from "./ui";

export function ResourceRegistry({ id, canEdit, busy }: { id: string; canEdit: boolean; busy: boolean }) {
  const query = useQuery({ queryKey: ["resource-registry", id], queryFn: ({ signal }) => request<{ registry_id: string }>("GET", `/api/workloads/${id}/registry`, undefined, signal) });
  const registries = useQuery(connectionsQuery("registries"));
  const [selection, setSelection] = useState<string | null>(null);
  const client = useQueryClient();
  const online = useOnline();
  const save = useMutation({ mutationFn: (registry_id: string) => request("PUT", `/api/workloads/${id}/registry`, { registry_id }), onSuccess: async () => { await client.invalidateQueries({ queryKey: ["resource-registry", id] }); setSelection(null); } });
  if (query.isError || registries.isError) return <QueryError what="registry" error={query.error ?? registries.error} onRetry={() => { void query.refetch(); void registries.refetch(); }} />;
  return <Section width="form" title="Docker registry"><div className="space-y-3">
    <Field id="resource-registry" label="Registry credentials" hint="Used for private image pulls and builds on the next deployment. Credentials are not placed in your Compose file."><Select id="resource-registry" className={inputClass} value={selection ?? query.data?.registry_id ?? ""} disabled={!canEdit || busy || !query.data || !registries.data} onChange={(event) => setSelection(event.target.value)}><option value="">Public images (no credentials)</option>{registries.data?.map((registry) => <option key={registry.id} value={registry.id}>{registry.name}</option>)}</Select></Field>
    {canEdit && <Button onClick={() => save.mutate(selection ?? "")} pending={save.isPending} disabled={!online || busy || selection === null}>Save registry</Button>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}{save.isSuccess && selection === null && <p className="text-sm text-fg-muted">Registry saved.</p>}
  </div></Section>;
}
