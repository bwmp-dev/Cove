import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { request, errorMessage } from "../api/client";
import { EnvironmentEditor } from "./EnvironmentEditor";
import { Button, Notice, Section } from "./ui";
import { UnsavedChangesGuard } from "./SaveBar";
import { useOnline } from "../lib/hooks";
import { validateEnvironmentKey, type EnvRow } from "../lib/workload-form";

export function SharedVariables({ scope, id, collapsible = false }: { scope: "team" | "project" | "environment"; id?: string; collapsible?: boolean }) {
  const path = scope === "team" ? "/api/shared-variables" : `/api/${scope}s/${encodeURIComponent(id ?? "")}/shared-variables`;
  const client = useQueryClient();
  const online = useOnline();
  const query = useQuery({ queryKey: ["shared-variables", path], queryFn: ({ signal }) => request<{ keys: string[] }>("GET", path, undefined, signal) });
  const [rows, setRows] = useState<EnvRow[]>([]);
  const [removed, setRemoved] = useState<string[]>([]);
  const [error, setError] = useState("");
  const save = useMutation({ mutationFn: (patch: Record<string, string | null>) => request("PATCH", path, { patch }), networkMode: "always", gcTime: 0,
    onSuccess: () => { setRows([]); setRemoved([]); void client.invalidateQueries({ queryKey: ["shared-variables", path] }); },
    onError: (cause) => setError(errorMessage(cause)),
    onSettled: (): void => { save.reset(); },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    const patch: Record<string, string | null> = Object.fromEntries(removed.map((key) => [key, null]));
    const seen = new Set<string>();
    for (const row of rows) {
      const invalid = validateEnvironmentKey(row.key);
      if (invalid || seen.has(row.key) || row.value.length > 65536 || row.value.includes("\0")) { setError(invalid || "Use unique names and values no longer than 64 KiB without null characters."); return; }
      seen.add(row.key); patch[row.key] = row.value;
    }
    setError(""); save.mutate(patch);
  }
  const dirty = rows.length > 0 || removed.length > 0;
  const count = query.data?.keys.length ?? 0;
  const summary = dirty ? "Unsaved changes" : query.isPending ? "Loading variable names" : query.isError ? "Could not load variables" : count === 0 ? "None saved" : `${count} saved ${count === 1 ? "variable" : "variables"}`;
  return <Section title="Shared variables" collapsed={collapsible ? { summary } : undefined} description={`Reference values with {{${scope}.NAME}} in a resource variable. Shared values are available to resources in this ${scope} and its descendants, including their editors. Changes apply on the next deploy.`}>
    <form onSubmit={submit} className="space-y-4">
      <UnsavedChangesGuard dirty={dirty} />
      {query.isPending && <p className="text-[13px] text-fg-muted">Loading variable names...</p>}
      {query.error && <Notice title="Could not load variables" tone="error">{errorMessage(query.error)}</Notice>}
      <fieldset disabled={save.isPending || !online} className="space-y-4">
        {Boolean(query.data?.keys.length) && <ul className="divide-y divide-line">{query.data?.keys.map((key) => <li key={key} className="flex items-center justify-between gap-3 py-2"><code className="break-all text-xs">{key}</code><Button size="sm" variant="ghost" onClick={() => setRemoved((current) => current.includes(key) ? current.filter((name) => name !== key) : [...current, key])}>{removed.includes(key) ? "Undo removal" : "Remove"}</Button></li>)}</ul>}
        <p className="text-xs text-fg-muted">Saved values are hidden. Enter an existing name to replace its value. References inside shared values are treated literally.</p>
        <EnvironmentEditor rows={rows} onChange={setRows} />
        {error && <Notice title="Could not save variables" tone="error">{error}</Notice>}
        <div className="flex gap-2"><Button type="submit" disabled={!dirty || query.isError} pending={save.isPending}>Save variables</Button><Button variant="ghost" disabled={!dirty} onClick={() => { setRows([]); setRemoved([]); setError(""); }}>Discard changes</Button></div>
      </fieldset>
    </form>
  </Section>;
}

