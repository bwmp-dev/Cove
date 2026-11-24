import { Checkbox } from "./Checkbox";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { nodesQuery } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { useSession } from "../lib/session";
import { managesTeam } from "../lib/permissions";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { ChevronRight } from "lucide-react";
import { countLabel } from "../lib/hierarchy";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Subsection, SwitchField } from "./ui";
import { CloudflareConnect } from "./CloudflareConnect";
import { zoneCertificateChallengesQuery } from "../api/certificates";
import { ChallengeList } from "./CertificateChallenges";

interface Share { team_id: string; domains: string[] }
interface Zone {
  connection_id?: string;
  preserve_existing_proxy?: boolean;
  can_manage: boolean; shares: Share[];
  zone_id: string; domain: string; enabled: boolean; proxied: boolean;
  nodes: Record<string, string>; error?: string; checked_at?: string;
}

function ZoneForm({ initial, onDone }: { initial?: Zone; onDone: () => void }) {
  const [preserveProxy, setPreserveProxy] = useState(initial?.preserve_existing_proxy ?? !initial);
  const client = useQueryClient();
  const online = useOnline();
  const nodes = useQuery(nodesQuery);
  const save = useMutation({ gcTime: 0,
    mutationFn: (zone: Omit<Zone, "can_manage" | "shares"> & { token: string }) => request("PUT", "/api/cloudflare", zone),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ["cloudflare"] }); onDone(); },
  });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const text = (key: string) => { const value = data.get(key); return typeof value === "string" ? value.trim() : ""; };
    const addresses: Record<string, string> = { ...initial?.nodes };
    for (const node of nodes.data ?? []) { const value = text(`node-${node.id}`); if (value) addresses[node.id] = value; else delete addresses[node.id]; }
    save.mutate({ zone_id: text("zone"), domain: text("domain"), token: text("token"), enabled: data.has("enabled"), proxied: data.has("proxied"), preserve_existing_proxy: preserveProxy, nodes: addresses });
  }
  return <Subsection title={initial ? `DNS settings for ${initial.domain}` : "Connect a Cloudflare zone"}>
  <form onSubmit={submit} className="min-w-0 space-y-5">
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id="cf-domain" label="Zone domain"><input id="cf-domain" name="domain" className={inputClass} defaultValue={initial?.domain} readOnly={!!initial} placeholder="example.com" required /></Field>
      <Field id="cf-zone" label="Cloudflare zone ID"><input id="cf-zone" name="zone" className={inputClass} defaultValue={initial?.zone_id} readOnly={!!initial} required /></Field>
    </div>
    {initial?.connection_id ? <p className="text-[13px] text-fg-muted">Uses your saved Cloudflare connection. Manage its credentials from the connections list.</p> : <Field id="cf-token" label="API token" hint="Requires Zone Read and DNS Edit for this zone. Leave blank to retain the saved token."><input id="cf-token" name="token" type="password" autoComplete="off" className={inputClass} required={!initial} /></Field>}
    <fieldset className="space-y-3"><legend className="text-[13px] font-medium text-fg">Worker public addresses</legend>
      <p className="text-xs text-fg-muted">Only workers with an address here are managed. Use the address that serves application traffic.</p>
      {nodes.isError && <QueryError what="nodes" error={nodes.error} onRetry={() => void nodes.refetch()} />}
      <div className="grid gap-3 sm:grid-cols-2">
        {nodes.data?.map(node => <Field key={node.id} id={`cf-${node.id}`} label={node.name} optional><input id={`cf-${node.id}`} name={`node-${node.id}`} className={inputClass} defaultValue={initial?.nodes[node.id]} placeholder="Public IPv4 or IPv6" /></Field>)}
      </div>
    </fieldset>
    <fieldset className="space-y-3"><legend className="mb-3 text-[13px] font-medium text-fg">Records</legend>
      <SwitchField id="cf-enabled" name="enabled" defaultChecked={initial?.enabled ?? true} label="Automatically correct DNS for domains explicitly shared with teams" description="Updates existing single-address records and creates missing records. Conflicting CNAMEs or multiple addresses require review. Turning this off leaves existing records in place." />
      <SwitchField id="cf-preserve-proxy" checked={preserveProxy} onChange={event => setPreserveProxy(event.target.checked)} label="Keep existing records’ Cloudflare proxy settings" />
      <SwitchField id="cf-proxied" name="proxied" defaultChecked={initial?.proxied} label={preserveProxy ? "Proxy newly created records through Cloudflare" : "Use Cloudflare proxy"} />
    </fieldset>
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="flex flex-wrap gap-2 border-t border-line pt-4"><Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>Save DNS settings</Button><Button variant="ghost" onClick={onDone} disabled={save.isPending}>Cancel</Button></div>
  </form>
  </Subsection>;
}

export function CloudflareSettings() {
  const zones = useQuery({ queryKey: ["cloudflare"], queryFn: ({ signal }) => request<Zone[]>("GET", "/api/cloudflare", undefined, signal), refetchInterval: 30_000 });
  const session = useSession();
  const [sharing, setSharing] = useState<Zone | null>(null);
  const [editing, setEditing] = useState<Zone | "new" | null>(null);
  const [challenges, setChallenges] = useState<Zone | null>(null);
  if (challenges) return <ZoneChallenges zone={challenges} onDone={() => setChallenges(null)} />;
  if (sharing) return <DomainSharing zone={sharing} onDone={() => setSharing(null)} />;
  if (editing) return <ZoneForm initial={editing === "new" ? undefined : editing} onDone={() => setEditing(null)} />;
  return <div className="divide-y divide-line">
    <CloudflareConnect />
    <Subsection title="Domains" description="The connection owner manages DNS automation and worker addresses, and chooses which domains each team can use.">
      {zones.isPending && <LoadingRows label="Loading domains" rows={2} />}
      {zones.isError && <QueryError what="DNS settings" error={zones.error} onRetry={() => void zones.refetch()} retrying={zones.isFetching} />}
      {zones.data?.length === 0 && <p className="text-[13px] text-fg-muted">No domains connected yet. Choose domains from a connection above.</p>}
      {Boolean(zones.data?.length) && <ul aria-label="Cloudflare domains" className="divide-y divide-line rounded-md border border-line">
        {zones.data?.map(zone => {
          const sharedHere = zone.shares.filter(share => share.team_id === session?.team_id).flatMap(share => share.domains);
          return <li key={zone.zone_id} className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 px-3 py-2">
            <div className="min-w-0 flex-1 basis-48">
              <p className="text-[13px] font-medium break-words text-fg">{zone.domain}</p>
              <div className="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-fg-muted">
                <StatusLabel tone={zone.error ? "bad" : zone.enabled ? "ok" : "neutral"} label={zone.enabled ? "Automatic DNS enabled" : "Automatic DNS disabled"} />
                {zone.can_manage && <span>{zone.shares.length === 0 ? "Not shared" : `Shared with ${countLabel(zone.shares.length, "team")}`}</span>}
              </div>
              {zone.error && <p role="alert" className="mt-1 text-xs break-words text-bad">{zone.error}</p>}
              {!zone.can_manage && <p className="mt-1 text-xs break-words text-fg-muted">Shared with this team: {sharedHere.join(", ")}</p>}
            </div>
            {zone.can_manage && <div className="flex flex-wrap gap-2"><Button size="sm" onClick={() => setEditing(zone)}>Edit</Button><Button size="sm" onClick={() => setSharing(zone)}>Share domains</Button><Button size="sm" variant="ghost" onClick={() => setChallenges(zone)} aria-label={`DNS challenges for ${zone.domain}`}>DNS challenges</Button></div>}
          </li>;
        })}
      </ul>}
    </Subsection>
    <details className="group p-4">
      <summary className="flex cursor-pointer items-center gap-1.5 rounded text-[13px] text-fg-muted hover:text-fg"><ChevronRight aria-hidden className="size-4 shrink-0 transition-transform group-open:rotate-90" />Advanced: connect a zone manually</summary>
      <div className="mt-3 space-y-2 pl-5.5"><p className="text-xs text-fg-muted">Use a zone ID and a token scoped to that zone instead of a saved connection.</p><Button onClick={() => setEditing("new")}>Connect Cloudflare zone</Button></div>
    </details>
  </div>;
}

// Owner and administrator view of every unresolved DNS-01 record in the zone, including cleanup for deleted resources.
function ZoneChallenges({ zone, onDone }: { zone: Zone; onDone: () => void }) {
  const challenges = useQuery(zoneCertificateChallengesQuery(zone.zone_id));
  return <Subsection
    title={`DNS challenge records in ${zone.domain}`}
    description="Temporary _acme-challenge TXT records Cove published for ACME DNS-01 certificates, and records it still has to remove, including those of removed issuers and deleted resources. Cove reconciles them automatically while the panel is online, also after a team's share is revoked. Records Cove does not own are never listed or removed."
    actions={<Button variant="ghost" onClick={onDone}>Close</Button>}
  >
    <div className="overflow-hidden rounded-md border border-line"><ChallengeList challenges={challenges} scope={{ kind: "zone" }} /></div>
  </Subsection>;
}

function DomainSharing({ zone, onDone }: { zone: Zone; onDone: () => void }) {
  const session = useSession();
  const online = useOnline();
  const cache = useQueryClient();
  const teams = session?.teams.filter(team => session.is_admin || managesTeam(team) || team.grants.some(grant => grant.scope === "team" && grant.role === "editor")) ?? [];
  const initial = Object.fromEntries(teams.map(team => [team.id, zone.shares.find(share => share.team_id === team.id)?.domains.join("\n") ?? ""]));
  const [domains, setDomains] = useState(initial);
  const dirty = teams.some(team => domains[team.id] !== initial[team.id]);
  const save = useMutation({
    mutationFn: () => request("PUT", `/api/cloudflare/${zone.zone_id}/shares`, { shares: teams.flatMap(team => {
      const patterns = (domains[team.id] ?? "").split(/\r?\n/).map(value => value.trim()).filter(Boolean);
      return patterns.length ? [{ team_id: team.id, domains: patterns }] : [];
    }) }),
    onSuccess: async () => { await cache.invalidateQueries({ queryKey: ["cloudflare"] }); onDone(); },
    networkMode: "always",
  });
  return <Subsection
    title={`Share domains from ${zone.domain}`}
    description="Give a team the entire domain, including all subdomains, or specific hostnames. Only teams where you can manage integrations are listed."
    actions={<Button variant="ghost" disabled={save.isPending || dirty} onClick={onDone}>Close</Button>}
  >
  <form className="space-y-4" onSubmit={event => { event.preventDefault(); if (online && dirty && !save.isPending) save.mutate(); }}>
    <UnsavedChangesGuard dirty={dirty && !save.isSuccess} />
    {teams.length > 0 && <ul aria-label="Teams" className="divide-y divide-line rounded-md border border-line">
      {teams.map(team => {
        const patterns = (domains[team.id] ?? "").split(/\r?\n/).map(value => value.trim());
        const full = patterns.includes(zone.domain) && patterns.includes(`*.${zone.domain}`);
        return <li key={team.id} className="grid min-w-0 gap-x-4 gap-y-2 px-3 py-2.5 sm:grid-cols-[minmax(0,12rem)_minmax(0,1fr)] sm:items-start">
          <label className="flex min-h-9 items-center gap-2.5 text-[13px]"><Checkbox aria-label={`Full domain access for ${team.name}`} checked={full} disabled={save.isPending} onChange={event => { setDomains(current => ({ ...current, [team.id]: event.target.checked ? `${zone.domain}\n*.${zone.domain}` : "" })); save.reset(); }} /><span className="min-w-0"><span className="block font-medium break-words text-fg">{team.name}</span><span className="block text-xs text-fg-muted">Full domain access</span></span></label>
          {full ? <p className="text-xs break-words text-fg-muted sm:py-2">{zone.domain} and every subdomain</p> : <Field id={`cf-share-${team.id}`} label={team.name} hint="Specific hostnames or *.subdomain patterns, one per line. Blank means no access."><textarea id={`cf-share-${team.id}`} className={inputClass} rows={2} value={domains[team.id] ?? ""} disabled={save.isPending} onChange={event => { setDomains(current => ({ ...current, [team.id]: event.target.value })); save.reset(); }} placeholder={`app.${zone.domain}`} aria-describedby={`cf-share-${team.id}-hint`} /></Field>}
        </li>;
      })}
    </ul>}
    {teams.length === 0 && <p className="text-[13px] text-fg-muted">Team-wide editing access is required to share this connection.</p>}
    <p className="text-xs text-fg-muted">Removing access stops future DNS updates for that team. Existing DNS records remain. Team members cannot read the API token or share this connection onward.</p>
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="border-t border-line pt-4"><SaveBar dirty={dirty} pending={save.isPending} blocked={!online} onDiscard={() => { setDomains(initial); save.reset(); }} /></div>
  </form>
  </Subsection>;
}
