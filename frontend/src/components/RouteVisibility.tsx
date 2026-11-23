import type { Listener } from "../api/ingress-listeners";
import type { RouteVisibility } from "../api/types";
import { describedBy } from "../lib/styles";
import { isPrivateVisibility, listenerBindings, privateRanges, visibilityNames, type PolicyDraft, type RouteRow } from "../lib/workload-form";
import { Select } from "./Select";
import { Field, QueryError } from "./ui";

// The node's managed listeners as the Domains form sees them. routeProtocol: the node worker's route protocol, undefined
// while unknown; private routes need 5. list: undefined while loading, after a failure or when the worker is older.
export interface ListenerChoices {
  routeProtocol: number | undefined;
  list: Listener[] | undefined;
  error: unknown;
  retry: () => void;
  retrying: boolean;
  nodeName: string;
  // Links to the node's Proxy page, where instance administrators manage listeners; omitted while the node is unknown.
  nodeId?: string;
  // The node could not be read, so routeProtocol is unknown because of a failure rather than still loading.
  nodeReadFailed: boolean;
}

type Change = (patch: Partial<Pick<PolicyDraft, "visibility" | "listener">>) => void;

function visibilityHint(visibility: PolicyDraft["visibility"], choices: ListenerChoices | undefined) {
  const current = isPrivateVisibility(visibility);
  if (!choices) {
    return current
      ? "Saved as a private route. Private listeners can be chosen once the resource exists, on its Domains page."
      : "Public serves this hostname on the node's public HTTP and HTTPS ports. To serve it only on a LAN or Tailscale listener, create the resource first, then set its visibility on the Domains page.";
  }
  if (choices.routeProtocol === undefined) return choices.nodeReadFailed ? "Private routes can be changed once the worker capabilities of this node can be read." : "Checking whether the worker on this node supports private routes.";
  if (choices.routeProtocol < 5) {
    return current
      ? "Private routes need a newer worker on this node. The saved setting is kept but cannot be changed here, and switching to Public is also checked against the worker. Upgrade it and verify the node again."
      : "LAN and Tailscale visibility need a newer worker on this node. Upgrade it and verify the node again.";
  }
  if (!current && choices.list === undefined) {
    return choices.error != null
      ? `LAN and Tailscale need the listeners of ${choices.nodeName}, which could not be read. Retry below to choose them.`
      : `Reading the listeners of ${choices.nodeName}. LAN and Tailscale can be chosen once they are loaded.`;
  }
  return current ? `Served only on the selected ${visibilityNames[visibility]} listener of ${choices.nodeName}.` : `Public serves this hostname on the public HTTP and HTTPS ports of ${choices.nodeName}. LAN and Tailscale serve it only on a private listener.`;
}

function listenerReadFailed(choices: ListenerChoices | undefined): choices is ListenerChoices {
  return choices !== undefined && (choices.routeProtocol ?? 0) >= 5 && choices.list === undefined && choices.error != null;
}

function listenerPrompt(choices: ListenerChoices | undefined, matching: Listener[]) {
  if (!choices || (choices.routeProtocol ?? 0) < 5) return "Listeners unavailable";
  if (choices.list === undefined) return choices.error != null ? "Listeners could not be loaded" : "Loading listeners…";
  return matching.length === 0 ? "No matching listener" : "Choose a listener";
}

// savedVisibility: the visibility saved for this hostname, so choosing Public keeps an explicitly saved "public" and
// otherwise leaves the field omitted. A saved private choice is never changed unless people choose another one.
export function RouteVisibilityFields({ id, route, policy, savedVisibility, errors, choices, onChange }: { id: string; route: RouteRow; policy: PolicyDraft; savedVisibility?: RouteVisibility; errors: Record<string, string>; choices?: ListenerChoices; onChange: Change }) {
  const visibility = policy.visibility || "public";
  const current = isPrivateVisibility(visibility) ? visibility : undefined;
  const list = choices && (choices.routeProtocol ?? 0) >= 5 ? choices.list : undefined;
  const visibilityError = errors[`${route.id}.visibility`];
  // A private route reports a failed read beside its listener, so only public routes show it here.
  const listenersFailed = !current && listenerReadFailed(choices);
  const statusId = `${id}-listeners-status`;
  return (
    <div className="space-y-3">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-visibility`} label="Visibility" hint={visibilityHint(policy.visibility, choices)} error={visibilityError}>
          <Select
            id={`${id}-visibility`}
            value={visibility}
            onChange={(event) => {
              const value = event.target.value;
              if (value === "lan" || value === "tailscale") {
                const keep = list?.some((item) => item.id === policy.listener && item.protocol === "https" && item.visibility === value);
                onChange({ visibility: value, listener: keep ? policy.listener : "" });
              } else onChange({ visibility: savedVisibility === "public" ? "public" : "", listener: "" });
            }}
            aria-invalid={visibilityError ? true : undefined}
            aria-describedby={[describedBy(`${id}-visibility`, visibilityError, true), listenersFailed ? statusId : undefined].filter(Boolean).join(" ")}
          >
            <option value="public">Public</option>
            <option value="lan" disabled={current !== "lan" && list === undefined}>
              LAN only
            </option>
            <option value="tailscale" disabled={current !== "tailscale" && list === undefined}>
              Tailscale only
            </option>
          </Select>
        </Field>
        {current && <ListenerField id={`${id}-listener`} visibility={current} listener={policy.listener} list={list} choices={choices} error={errors[`${route.id}.listener`]} onChange={(listener) => onChange({ listener })} />}
      </div>
      {listenersFailed && (
        <div id={statusId}>
          <QueryError error={choices.error} what="route listeners" onRetry={choices.retry} retrying={choices.retrying} />
        </div>
      )}
      {current && <PrivateRouteDetails visibility={current} listener={policy.listener} list={list} choices={choices} host={route.domain.trim() || "this hostname"} />}
    </div>
  );
}

function matchingListeners(list: Listener[] | undefined, visibility: "lan" | "tailscale") {
  return (list ?? []).filter((item) => item.protocol === "https" && item.visibility === visibility);
}

// list: the node's listeners when the worker can report them; a saved listener missing from it stays selectable so it
// is never replaced silently.
function ListenerField({ id, visibility, listener, list, choices, error, onChange }: { id: string; visibility: "lan" | "tailscale"; listener: string; list?: Listener[]; choices?: ListenerChoices; error?: string; onChange: (listener: string) => void }) {
  const matching = matchingListeners(list, visibility);
  const unavailable = listener !== "" && !matching.some((item) => item.id === listener) ? listener : "";
  return (
    <Field id={id} label="Listener" error={error} hint={`${visibilityNames[visibility]} HTTPS listeners of the node. An instance administrator configures them on the node's Proxy page.`}>
      <Select id={id} value={listener} disabled={list === undefined} onChange={(event) => onChange(event.target.value)} aria-invalid={error ? true : undefined} aria-describedby={describedBy(id, error, true)}>
        {listener === "" && (
          <option value="" disabled>
            {listenerPrompt(choices, matching)}
          </option>
        )}
        {unavailable && <option value={unavailable}>{`${unavailable} (${list ? "unavailable" : "saved"})`}</option>}
        {matching.map((item) => (
          <option key={item.id} value={item.id}>
            {`${item.id} · ${listenerBindings(item)}`}
          </option>
        ))}
      </Select>
    </Field>
  );
}

function ListenerStatus({ visibility, listener, list, choices }: { visibility: "lan" | "tailscale"; listener: string; list?: Listener[]; choices?: ListenerChoices }) {
  if (choices && list === undefined && choices.error != null) return <QueryError error={choices.error} what="route listeners" onRetry={choices.retry} retrying={choices.retrying} />;
  if (!list) return null;
  const matching = matchingListeners(list, visibility);
  const selected = matching.find((item) => item.id === listener);
  if (selected) {
    return (
      <p className="text-xs text-fg-muted">
        Bound to <span className="font-mono break-all text-fg">{listenerBindings(selected)}</span> on {choices?.nodeName}.
      </p>
    );
  }
  if (listener !== "") {
    return (
      <p className="text-xs text-warn">
        Listener <span className="font-mono">{listener}</span> is not a {visibilityNames[visibility]} HTTPS listener on this node any more. Choose another listener or switch to Public; saving it unchanged is rejected.
      </p>
    );
  }
  if (matching.length > 0) return null;
  return (
    <p className="text-xs text-fg-muted">
      No {visibilityNames[visibility]} HTTPS listener is configured on {choices?.nodeName}. An instance administrator can add one on the node&apos;s Proxy page.
    </p>
  );
}

function PrivateRouteDetails({ visibility, listener, list, choices, host }: { visibility: "lan" | "tailscale"; listener: string; list?: Listener[]; choices?: ListenerChoices; host: string }) {
  return (
    <>
      <ListenerStatus visibility={visibility} listener={listener} list={list} choices={choices} />
      <div className="space-y-1 text-xs text-fg-muted">
        <p>
          Only clients connecting from {privateRanges[visibility]} are allowed. The proxy checks the connecting address, so a client behind NAT or another proxy may arrive from a different address. Allowed source IP ranges and authentication set below still apply on top.
        </p>
        <p>
          Every path on <span className="font-mono break-all">{host}</span> is refused with status 418 on the public HTTP and HTTPS ports, and port 80 may redirect before refusing. Certificate names remain visible during TLS. No public Cloudflare DNS record is created: point LAN or Tailscale DNS at the listener address yourself. Firewall rules and Tailscale ACLs are not changed.
        </p>
      </div>
    </>
  );
}
