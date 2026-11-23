import { useMutation, useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RotateCcw } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { keys, workloadQuery, workloadsQuery } from "../api/queries";
import type { Route, Workload } from "../api/types";
import { Checkbox } from "../components/Checkbox";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { RelativeTime } from "../components/RelativeTime";
import { Select } from "../components/Select";
import { StatusLabel } from "../components/status";
import { Button, DefinitionList, EmptyState, Field, Notice, QueryError, Section } from "../components/ui";
import { isWildcardHostname } from "../lib/certificates";
import { useOnline } from "../lib/hooks";
import { can, currentTeam, managesTeam } from "../lib/permissions";
import { useSession } from "../lib/session";
import { buttonClass, inputClass, linkClass } from "../lib/styles";
import { useResource } from "./workload-detail";

interface ObjectBucket {
  id: string;
  storage_resource_id: string;
  storage_id: string;
  bucket: string;
  endpoint: string;
  region: string;
  state: "pending" | "ready";
  created_at: string;
  warning: string;
}

const boundKeys = ["S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_REGION"];
// The add-on only accepts the S3 API route of the unmodified Garage template; the administration API stays private.
const garageService = "garage";
const garagePort = 3900;

const bucketsKey = (id: string) => ["workloads", id, "object-buckets"] as const;
const path = (id: string) => `/api/workloads/${encodeURIComponent(id)}/object-buckets`;

export function ResourceObjectStorage() {
  const workload = useResource();
  if (workload.database || workload.preview_source_id) {
    return <EmptyState title="Object storage is not available here">Buckets are provisioned for application resources. Managed databases and previews cannot receive one.</EmptyState>;
  }
  return <ObjectStorage workload={workload} />;
}

function ObjectStorage({ workload }: { workload: Workload }) {
  const session = useSession();
  const admin = Boolean(session?.is_admin) || managesTeam(currentTeam(session));
  const manager = admin && can(workload.permissions, "edit");
  const buckets = useQuery({
    queryKey: bucketsKey(workload.id),
    queryFn: ({ signal }) => request<ObjectBucket[]>("GET", path(workload.id), undefined, signal),
    refetchInterval: (query) => (query.state.data?.some((bucket) => bucket.state === "pending") ? 5_000 : false),
  });
  const bucket = buckets.data?.[0];
  return (
    <Section width="form" title="Object storage" description="A private S3 bucket in a Garage resource you run, bound to this resource's S3 variables. One bucket per resource.">
      {buckets.isPending ? (
        <p className="text-[13px] text-fg-muted">Loading object storage</p>
      ) : buckets.isError ? (
        <QueryError what="object storage" error={buckets.error} onRetry={() => void buckets.refetch()} retrying={buckets.isFetching} />
      ) : bucket ? (
        <BucketDetails workload={workload} bucket={bucket} manager={manager} />
      ) : manager ? (
        <ProvisionForm workload={workload} />
      ) : (
        <p className="text-[13px] text-fg-muted">
          No bucket is provisioned. A team owner or admin with editor access to this resource, and edit and read secrets access to the Garage resource, can provision one.
        </p>
      )}
    </Section>
  );
}

function BucketDetails({ workload, bucket, manager }: { workload: Workload; bucket: ObjectBucket; manager: boolean }) {
  const client = useQueryClient();
  const online = useOnline();
  const resources = useQuery(workloadsQuery);
  const [confirm, setConfirm] = useState(false);
  const refresh = () => Promise.all([client.invalidateQueries({ queryKey: bucketsKey(workload.id) }), client.invalidateQueries({ queryKey: keys.workload(workload.id) }), client.invalidateQueries({ queryKey: ["connections", "storage-locations"] })]);
  const retry = useMutation({ mutationFn: () => request<{ id: string; state: string }>("POST", `${path(workload.id)}/${encodeURIComponent(bucket.id)}/retry`, {}), onSettled: refresh });
  const revoke = useMutation({
    mutationFn: () => request<{ ok: boolean; data_retained: boolean }>("DELETE", `${path(workload.id)}/${encodeURIComponent(bucket.id)}`, { confirm: true }),
    onSuccess: () => setConfirm(false),
    onSettled: refresh,
  });
  const storage = resources.data?.find((item) => item.id === bucket.storage_resource_id);
  const pending = bucket.state === "pending";
  return (
    <div className="space-y-4">
      <Notice tone="warn" title="Not offsite">
        {bucket.warning}
      </Notice>
      <DefinitionList
        items={[
          { term: "State", value: <StatusLabel tone={pending ? "warn" : "ok"} label={pending ? "Provisioning not confirmed" : "Ready"} /> },
          {
            term: "Garage resource",
            value: storage ? (
              <Link to="/resources/$resourceId" params={{ resourceId: storage.id }} className={linkClass}>
                {storage.project ? `${storage.project} / ` : ""}
                {storage.name}
              </Link>
            ) : (
              "Resource you cannot view"
            ),
          },
          { term: "Bucket", value: <span className="font-mono text-xs break-all">{bucket.bucket}</span> },
          { term: "Endpoint", value: <span className="font-mono text-xs break-all">{bucket.endpoint}</span> },
          { term: "Region", value: <span className="font-mono text-xs">{bucket.region}</span> },
          { term: "Created", value: <RelativeTime value={bucket.created_at} /> },
          { term: "Backup destination", value: bucket.storage_id ? <Link to="/storage" className={linkClass}>Team storage location</Link> : "None" },
        ]}
      />
      {pending ? (
        <Notice tone="warn" title="Retry the recorded request">
          The worker did not confirm provisioning. Retry finishes the same bucket and key; conflicting changes to this resource and the Garage resource stay locked until it succeeds.
        </Notice>
      ) : (
        <div className="space-y-1.5">
          <p className="text-[13px] text-fg">Bound variables, applied on this resource's next deploy:</p>
          <ul aria-label="Bound S3 variables" className="flex flex-wrap gap-x-4 gap-y-1">
            {boundKeys.map((key) => (
              <li key={key} className="font-mono text-xs text-fg-muted">
                {key}
              </li>
            ))}
          </ul>
          <p className="text-xs text-fg-muted">The key reads and writes this bucket only. Its secret is never shown here; it is delivered through the binding.</p>
        </div>
      )}
      {retry.isError && <Notice tone="error" title="Retry failed">{errorMessage(retry.error)}</Notice>}
      {revoke.isError && <Notice tone="error" title="Could not revoke">{errorMessage(revoke.error)}</Notice>}
      {manager && (
        <div className="flex flex-wrap justify-end gap-2">
          {pending && (
            <Button variant="primary" disabled={!online} pending={retry.isPending} onClick={() => retry.mutate()}>
              <RotateCcw aria-hidden className="size-3.5" />
              Retry provisioning
            </Button>
          )}
          <Button variant="danger" disabled={!online || retry.isPending} onClick={() => setConfirm(true)}>
            Revoke access
          </Button>
        </div>
      )}
      <ConfirmDialog open={confirm} title="Revoke bucket access?" confirmLabel="Revoke access" danger pending={revoke.isPending} onCancel={() => setConfirm(false)} onConfirm={() => revoke.mutate()}>
        Cove deletes the scoped key and removes the bindings it created. Applications using these credentials stop working after their next deploy or when the key is checked. The bucket and its data are kept in Garage for recovery or deliberate deletion. Move backup policies and other bindings to this bucket first.
      </ConfirmDialog>
    </div>
  );
}

function ProvisionForm({ workload }: { workload: Workload }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const resources = useQuery(workloadsQuery);
  const [storage, setStorage] = useState("");
  const [domain, setDomain] = useState("");
  const [sameNode, setSameNode] = useState(false);
  const [backup, setBackup] = useState(false);
  const detail = useQuery({ ...workloadQuery(storage), enabled: Boolean(storage) });
  const provision = useMutation({
    mutationFn: (endpoint: string) => request<{ id: string; state: string }>("POST", path(workload.id), { storage_resource_id: storage, domain: endpoint, confirm_same_node: true, backup_destination: backup }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: bucketsKey(workload.id) }), client.invalidateQueries({ queryKey: keys.workload(workload.id) }), client.invalidateQueries({ queryKey: ["connections", "storage-locations"] })]),
  });
  const candidates = (resources.data ?? []).filter((item) => item.id !== workload.id && item.kind !== "database" && can(item.permissions, "edit") && can(item.permissions, "read_secrets"));
  // A wildcard hostname is not an address clients can connect to, so it is never offered as the S3 endpoint.
  const routes = (detail.data?.routes ?? []).filter((route) => route.service === garageService && route.port === garagePort && !isWildcardHostname(route.domain));
  const garage = detail.data ? /dxflrs\/garage:/.test(detail.data.compose) : false;
  const selectedDomain = routes.some((route) => route.domain === domain) ? domain : (routes[0]?.domain ?? "");

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!storage || !selectedDomain || !sameNode) return;
    provision.mutate(selectedDomain);
  }

  return (
    <form className="space-y-4" onSubmit={submit}>
      <p className="text-[13px] text-fg">
        Deploy the unmodified Garage template from the{" "}
        <Link to="/resources/new" className={linkClass}>
          resource templates
        </Link>
        , add an HTTPS domain for service <span className="font-mono text-xs">garage</span> port <span className="font-mono text-xs">3900</span> on its Domains page, then select it here. Do not route the administration port.
      </p>
      <Field id={`${id}-storage`} label="Garage resource">
        <Select
          id={`${id}-storage`}
          className={inputClass}
          value={storage}
          required
          onChange={(event) => {
            setStorage(event.target.value);
            setDomain("");
            provision.reset();
          }}
        >
          <option value="">Select resource</option>
          {candidates.map((item) => (
            <option key={item.id} value={item.id}>
              {item.project ? `${item.project} / ` : ""}
              {item.environment ? `${item.environment} / ` : ""}
              {item.name}
            </option>
          ))}
        </Select>
      </Field>
      {resources.isError && <QueryError what="resources" error={resources.error} onRetry={() => void resources.refetch()} retrying={resources.isFetching} />}
      {storage && <GarageEndpoint detail={detail} routes={routes} garage={garage} selected={selectedDomain} onSelect={setDomain} />}
      <Notice tone="warn" title="Same-node storage is not offsite">
        Garage may run on the same node as this resource, now or after topology changes. A failure of that node can lose both the data and its copy. Keep another backup in an independent failure domain.
      </Notice>
      <label className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox checked={sameNode} onChange={(event) => setSameNode(event.target.checked)} />
        <span>I understand this storage is not offsite.</span>
      </label>
      <label className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox checked={backup} onChange={(event) => setBackup(event.target.checked)} />
        <span>
          Also add the bucket as a team backup destination
          <span className="mt-0.5 block text-xs text-fg-muted">Created after provisioning succeeds and offered in backup policies. No policy is configured automatically.</span>
        </span>
      </label>
      {provision.isError && (
        <Notice tone="error" title={isStatus(provision.error, 502) ? "Provisioning was not confirmed" : "Could not provision the bucket"}>
          {errorMessage(provision.error)}
          {isStatus(provision.error, 502) ? " The request is retained; retry it from this page instead of provisioning again." : ""}
        </Notice>
      )}
      <p className="text-xs text-fg-muted">Cove creates a bucket and a key limited to it, then binds {boundKeys.join(", ")}. Existing variables with these names are not overwritten. Deploy this resource afterwards to use them.</p>
      <div className="flex justify-end">
        <Button type="submit" variant="primary" disabled={!online || !storage || !garage || !selectedDomain || !sameNode} pending={provision.isPending}>
          Provision bucket
        </Button>
      </div>
    </form>
  );
}

function GarageEndpoint({ detail, routes, garage, selected, onSelect }: { detail: UseQueryResult<Workload>; routes: Route[]; garage: boolean; selected: string; onSelect: (domain: string) => void }) {
  const id = useId();
  return (
    <>
      {detail.isPending && <p className="text-xs text-fg-muted">Checking the selected resource</p>}
      {detail.isError && <QueryError what="the selected resource" error={detail.error} onRetry={() => void detail.refetch()} retrying={detail.isFetching} />}
      {detail.data && !garage && (
        <Notice tone="warn" title="Not a Garage resource">
          Select a resource created from the Garage template without Compose changes.
        </Notice>
      )}
      {detail.data && garage && routes.length === 0 && (
        <Notice
          tone="warn"
          title="Add the S3 route first"
          action={
            <Link to="/resources/$resourceId/domains" params={{ resourceId: detail.data.id }} className={buttonClass("secondary")}>
              Open domains
            </Link>
          }
        >
          {detail.data.name} has no exact HTTPS domain for service garage on port 3900. A wildcard domain cannot be the S3 endpoint. Add an exact domain, deploy, then return here.
        </Notice>
      )}
      {routes.length > 0 && (
        <Field id={`${id}-domain`} label="S3 endpoint" hint="Clients connect over HTTPS with certificate verification.">
          <Select id={`${id}-domain`} className={inputClass} value={selected} onChange={(event) => onSelect(event.target.value)}>
            {routes.map((route) => (
              <option key={route.domain} value={route.domain}>
                https://{route.domain}
              </option>
            ))}
          </Select>
        </Field>
      )}
    </>
  );
}
