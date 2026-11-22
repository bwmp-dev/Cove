import { KeyRound } from "lucide-react";
import { Select } from "../components/Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { request, errorMessage } from "../api/client";
import { workloadsQuery } from "../api/queries";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { CopyButton } from "../components/CopyButton";
import { Button, Field, LoadingRows, Notice, PageHeader, Section, SectionGrid } from "../components/ui";
import { useOnline } from "../lib/hooks";
import { inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { formatAbsolute } from "../lib/format";
import { can } from "../lib/permissions";

interface Token { id: string; name: string; workload_id: string; permission: "read" | "operate"; expires_at: string }
const permissionLabels: Record<Token["permission"], string> = { read: "Read", operate: "Read and operate" };
const tokenKey = ["tokens"] as const;

function CreateToken() {
  const workloads = useQuery(workloadsQuery);
  const client = useQueryClient();
  const online = useOnline();
  const [name, setName] = useState("");
  const [workload, setWorkload] = useState("");
  const [permission, setPermission] = useState("read");
  const selected = workloads.data?.find((item) => item.id === workload);
  const canOperate = can(selected?.permissions, "operate");
  const [days, setDays] = useState(30);
  const create = useMutation({
    gcTime: 0,
    mutationFn: () => request<{ token: string }>("POST", "/api/tokens", { name, workload_id: workload, permission, days }),
    onSuccess: () => client.invalidateQueries({ queryKey: tokenKey }),
  });
  const issued = useRef<HTMLInputElement>(null);
  const token = create.data?.token;
  // The form that held focus is replaced by the one-time token, so move focus to it once.
  useEffect(() => {
    if (token) issued.current?.focus();
  }, [token]);
  if (create.data) return <div className="min-w-0 space-y-3">
    <Notice title="Copy this token now">It will not be shown again. It grants access only to the selected resource.</Notice>
    <Field id="issued-token" label="New API token"><input id="issued-token" ref={issued} className={inputClass} readOnly type="password" value={create.data.token} /></Field>
    <div className="flex gap-2"><CopyButton text={create.data.token} label="Copy token" /><Button onClick={() => create.reset()}>Close</Button></div>
  </div>;
  return <form className="min-w-0 space-y-4" onSubmit={(event) => { event.preventDefault(); create.mutate(); }}>
    <Field id="token-name" label="Name"><input id="token-name" className={inputClass} required maxLength={120} value={name} onChange={(event) => setName(event.target.value)} /></Field>
    <Field id="token-workload" label="Resource"><Select id="token-workload" className={inputClass} required value={workload} onChange={(event) => { setWorkload(event.target.value); setPermission("read"); }}><option value="">Select resource</option>{workloads.data?.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</Select></Field>
    {workloads.isError && <Notice tone="error" title="Could not load resources">{errorMessage(workloads.error)}</Notice>}
    <Field id="token-permission" label="Permission" hint={canOperate || !selected ? "Operate also permits deploy, rollback, start, stop, restart and cancellation. Neither permission allows editing configuration or managing nodes or tokens." : "Your access to this resource is read-only, so tokens for it can only read."}><Select id="token-permission" aria-describedby="token-permission-hint" className={inputClass} value={permission} onChange={(event) => setPermission(event.target.value)}><option value="read">Read resource and logs</option>{(canOperate || !selected) && <option value="operate" disabled={!selected}>Read and operate resource</option>}</Select></Field>
    <Field id="token-days" label="Expires in days"><input id="token-days" className={inputClass} type="number" required min={1} max={365} value={days} onChange={(event) => setDays(event.target.valueAsNumber)} /></Field>
    {create.isError && <Notice tone="error" title="Could not create token">{errorMessage(create.error)}</Notice>}
    <div className="flex justify-end"><Button type="submit" variant="primary" disabled={!online || create.isPending || workloads.isPending || workloads.isError}>Create token</Button></div>
  </form>;
}

export function TokensPage() {
  const client = useQueryClient();
  const online = useOnline();
  const tokens = useQuery({ queryKey: tokenKey, queryFn: ({ signal }) => request<Token[]>("GET", "/api/tokens", undefined, signal) });
  const workloads = useQuery(workloadsQuery);
  const [selected, setSelected] = useState<Token | null>(null);
  const revoke = useMutation({ mutationFn: (id: string) => request("DELETE", `/api/tokens/${encodeURIComponent(id)}`), onSuccess: () => client.invalidateQueries({ queryKey: tokenKey }) });
  return <>
    <PageHeader icon={<KeyRound aria-hidden />} title="API tokens" description="Give automation access to one resource in this team, with a fixed permission and expiry. Tokens never exceed your own access." />
    <SectionGrid>
      <Section title="Create token"><CreateToken /></Section>
      <Section title="Existing tokens" flush>
        {tokens.isPending && <LoadingRows label="Loading tokens" />}
        {(tokens.isError || revoke.isError) && <div className="space-y-2 p-3">
          {tokens.isError && <Notice tone="error" title="Could not load tokens">{errorMessage(tokens.error)}</Notice>}
          {revoke.isError && <Notice tone="error" title="Could not revoke token">{errorMessage(revoke.error)}</Notice>}
        </div>}
        {tokens.data?.length === 0 && <p className="px-4 py-3 text-[13px] text-fg-muted">No API tokens created.</p>}
        {tokens.data && tokens.data.length > 0 && <table className="w-full text-left">
          <caption className="sr-only">Existing tokens</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="pr-4 pl-4">Name</th>
              <th scope="col" className="hidden pr-4 sm:table-cell">Permission</th>
              <th scope="col" className="hidden pr-4 text-right md:table-cell">Expires</th>
              <th scope="col" className="pr-4 text-right"><span className="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {tokens.data.map((token) => <tr key={token.id} className={tableRowClass}>
              <td className="py-2 pr-4 pl-4">
                <p className="text-[13px] font-medium break-words text-fg">{token.name}</p>
                <p className="text-xs break-words text-fg-muted">
                  {workloads.data?.find((item) => item.id === token.workload_id)?.name ?? token.workload_id}
                  <span className="sm:hidden"> · {permissionLabels[token.permission]}</span>
                  <span className="md:hidden"> · Expires {formatAbsolute(token.expires_at)}</span>
                </p>
              </td>
              <td className="hidden py-2 pr-4 text-[13px] text-fg-muted sm:table-cell">{permissionLabels[token.permission]}</td>
              <td className="hidden py-2 pr-4 text-right text-[13px] whitespace-nowrap text-fg-muted tabular-nums md:table-cell">{formatAbsolute(token.expires_at)}</td>
              <td className="py-2 pr-4 text-right">
                <Button size="sm" variant="danger" disabled={!online || revoke.isPending} onClick={() => setSelected(token)}>Revoke<span className="sr-only"> {token.name}</span></Button>
              </td>
            </tr>)}
          </tbody>
        </table>}
      </Section>
    </SectionGrid>
    <ConfirmDialog open={selected !== null} title="Revoke this API token?" confirmLabel="Revoke token" danger onCancel={() => setSelected(null)} onConfirm={() => { if (selected) revoke.mutate(selected.id); setSelected(null); }}>Automation using this token will lose access immediately. Already accepted operations will continue.</ConfirmDialog>
  </>;
}
