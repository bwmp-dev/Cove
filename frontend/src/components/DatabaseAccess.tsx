import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { request, errorMessage } from "../api/client";
import { databaseDetailsQuery } from "../api/databases";
import { workloadOperationsQuery } from "../api/queries";
import type { DatabaseResource } from "../api/types";
import { adminTool, engineName, type AdminTool } from "../lib/databases";
import { formText } from "../lib/form-data";
import { useOnline } from "../lib/hooks";
import { activeOperation } from "../lib/operations";
import { choiceClass, inputClass, linkClass } from "../lib/styles";
import { Button, Field, Notice, Section } from "./ui";
import { CopyButton } from "./CopyButton";
import { ExpiryTime } from "./RelativeTime";

interface Lease { id: string; service: string; port: number; expires_at: string; tool?: string }
interface Connection { id: string; expires_at: string; host: string; ssh_port: number; host_key: string }
interface OpenRequest { service: string; port: number; public_key: string; tool?: string }
// "native" forwards the database port; any other value is the id of a browser admin tool.
type Mode = string;
interface OpenedAccess { value: Connection; tool?: AdminTool; port: number }
const nativeLocalPort = 15432;

// Typed databases always target their own service; a helper tool listens on its fixed port beside the database.
function accessRequest(data: FormData, tool: AdminTool | undefined, database?: DatabaseResource): OpenRequest {
  const public_key = formText(data, "public_key");
  if (!database) return { service: formText(data, "service"), port: Number(data.get("port")), public_key };
  if (tool) return { service: database.service, port: tool.port, public_key, tool: tool.id };
  return { service: database.service, port: database.port, public_key };
}

const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;

export function DatabaseAccess({ id, database }: { id: string; database?: DatabaseResource }) {
  const client = useQueryClient();
  const online = useOnline();
  const [connection, setConnection] = useState<OpenedAccess | null>(null);
  const [mode, setMode] = useState<Mode>("native");
  const details = useQuery({ ...databaseDetailsQuery(id), enabled: Boolean(database) });
  const operations = useQuery(workloadOperationsQuery(id));
  const busy = Boolean(operations.data && activeOperation(operations.data, id));
  const tools = (details.data?.admin_tools ?? []).map(adminTool).filter((tool): tool is AdminTool => tool !== undefined);
  const selectedTool = mode === "native" ? undefined : adminTool(mode);
  const key = ["database-access", id];
  const path = `/api/workloads/${id}/database-access`;
  const leases = useQuery({ queryKey: key, queryFn: ({ signal }) => request<Lease[]>("GET", path, undefined, signal), refetchInterval: 10000 });
  const open = useMutation({
    gcTime: 0,
    mutationFn: (body: OpenRequest) => request<Connection>("POST", path, body),
    onSuccess: (data, body) => {
      setConnection({ value: data, tool: adminTool(body.tool), port: body.port });
      return client.invalidateQueries({ queryKey: key });
    },
  });
  const close = useMutation({ mutationFn: (lease: string) => request("DELETE", path, { id: lease }), onSuccess: () => { setConnection(null); return client.invalidateQueries({ queryKey: key }); } });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    open.mutate(accessRequest(new FormData(event.currentTarget), selectedTool, database));
  }

  const error = open.error ?? close.error ?? leases.error;
  return (
    <Section
      width="form"
      title="Temporary database access"
      description="Open a 15-minute SSH connection to one running service. No database port is published. Expiry and revocation close active connections. The database still requires its own credentials."
    >
      <div className="space-y-4">
        <p className="text-sm text-fg-muted">
          Generate a dedicated Ed25519 key with ssh-keygen for each access, then paste its public key here. Run the Cove CLI locally with OpenSSH available. It binds only 127.0.0.1 and pins the worker&apos;s host key.
          This requires an SSH-enrolled worker reachable from your computer.
        </p>
        <form onSubmit={submit} className="space-y-3">
          {database ? <DatabaseTarget database={database} tools={tools} tool={selectedTool} onMode={setMode} /> : <ServiceTarget />}
          <Field id="access-key" label="Ed25519 public key">
            <textarea id="access-key" name="public_key" className={inputClass} rows={2} maxLength={8192} required />
          </Field>
          <Button type="submit" disabled={!online || busy} pending={open.isPending}>
            {selectedTool ? `Open ${selectedTool.name} access` : "Open temporary access"}
          </Button>
          {busy && <p className="text-xs text-fg-muted">Available when the active operation finishes.</p>}
        </form>
        {error && <Notice tone="error">{errorMessage(error)}</Notice>}
        {connection && <ConnectionInstructions id={id} database={database} connection={connection} />}
        {leases.data && leases.data.length > 0 && <LeaseList leases={leases.data} disabled={!online || close.isPending} onRevoke={(lease) => close.mutate(lease)} />}
      </div>
    </Section>
  );
}

function ConnectionInstructions({ id, database, connection }: { id: string; database?: DatabaseResource; connection: OpenedAccess }) {
  const { tool } = connection;
  const localPort = tool ? tool.localPort : nativeLocalPort;
  const { host, ssh_port, host_key, expires_at } = connection.value;
  const command = `cove connect --host ${quote(host)} --ssh-port ${ssh_port} --host-key ${quote(host_key.trim())} --expires ${quote(expires_at)} --local-port ${localPort} --identity ./cove-access-key`;
  let guidance = null;
  const databasePage = (
    <Link to="/resources/$resourceId/database" params={{ resourceId: id }} className={linkClass}>
      the Database page
    </Link>
  );
  if (tool && database) {
    guidance = (
      <ol className="list-decimal space-y-1 pl-5 text-[13px] text-fg">
        <li>
          Open <span className="font-mono text-xs">http://127.0.0.1:{tool.localPort}</span> in your browser.
        </li>
        {tool.id === "adminer" && (
          <>
            <li>
              The login form is preset to this database (<span className="font-mono text-xs">{database.service}</span>) and cannot reach any other server. Sign in with the credentials revealed on {databasePage}.
            </li>
          </>
        )}
        {tool.id === "mongo-express" && <li>When the browser asks for a user name and password, use the application credentials revealed on {databasePage}. The database connection is already configured.</li>}
        {tool.id === "redisinsight" && <li>On first use, Redis Insight asks you to review and accept its license and settings; Cove does not accept them for you. After you accept, it shows only this database's preconfigured connection.</li>}
      </ol>
    );
  } else if (database) {
    guidance = (
      <p className="text-xs text-fg-muted">
        Point the client at <span className="font-mono">127.0.0.1:{localPort}</span>. The forwarded container port is {connection.port}.
      </p>
    );
  }
  return (
    <div className="space-y-2">
      <p className="text-sm">Run locally, replacing the identity path and local port if needed:</p>
      <pre role="region" aria-label="Local database connection command" className="overflow-x-auto rounded border border-line p-3 text-xs" tabIndex={0}>
        {command}
      </pre>
      <CopyButton text={command} label="Copy connection command" />
      {guidance}
    </div>
  );
}

function LeaseList({ leases, disabled, onRevoke }: { leases: Lease[]; disabled: boolean; onRevoke: (lease: string) => void }) {
  return (
    <ul aria-label="Active access leases" className="divide-y divide-line">
      {leases.map((lease) => (
        <li key={lease.id} className="flex flex-wrap items-center justify-between gap-3 py-2">
          <div className="text-sm">
            {adminTool(lease.tool)?.name ?? <span className="font-mono text-xs">{lease.service}:{lease.port}</span>}
            <p className="text-xs text-fg-muted">
              <ExpiryTime value={lease.expires_at} />
            </p>
          </div>
          <Button size="sm" disabled={disabled} onClick={() => onRevoke(lease.id)}>
            Revoke access
          </Button>
        </li>
      ))}
    </ul>
  );
}

function DatabaseTarget({ database, tools, tool, onMode }: { database: DatabaseResource; tools: AdminTool[]; tool?: AdminTool; onMode: (mode: Mode) => void }) {
  const formId = useId();
  return (
    <>
      {tools.length > 0 && (
        <fieldset className="space-y-1.5">
          <legend className="mb-1.5 text-[13px] font-medium text-fg">Access</legend>
          <div className="flex flex-wrap gap-1.5">
            <label className={choiceClass}>
              <input type="radio" name={`${formId}-mode`} className="accent-fg" checked={!tool} onChange={() => onMode("native")} />
              Native client on port {database.port}
            </label>
            {tools.map((item) => (
              <label key={item.id} className={choiceClass}>
                <input type="radio" name={`${formId}-mode`} className="accent-fg" checked={tool?.id === item.id} onChange={() => onMode(item.id)} />
                {item.name} in the browser
              </label>
            ))}
          </div>
        </fieldset>
      )}
      <p className="text-xs text-fg-muted">
        {tool ? (
          <>
            Starts a temporary {tool.name} helper beside the database on container port {tool.port}, without a host port, and forwards it to your computer. Cove supplies its database connection, and it stops when access expires or is revoked. Browser tools also need permission to read secrets.
          </>
        ) : (
          <>
            Forwards <span className="font-mono">{database.service}:{database.port}</span> to your computer for {engineName(database.engine)} clients.
            {tools.length > 0 ? "" : " A browser admin tool is not offered for this engine."}
          </>
        )}
      </p>
    </>
  );
}

function ServiceTarget() {
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      <Field id="access-service" label="Database service">
        <input id="access-service" name="service" className={inputClass} required />
      </Field>
      <Field id="access-port" label="Container port">
        <input id="access-port" name="port" className={inputClass} type="number" min={1} max={65535} defaultValue={5432} required />
      </Field>
    </div>
  );
}
