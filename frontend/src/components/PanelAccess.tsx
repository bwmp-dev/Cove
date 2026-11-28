import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { panelAccessQuery, useUpdatePanelAccess, type PanelAccess } from "../api/instance-settings";
import { useOnline } from "../lib/hooks";
import { panelAllowlistLimit, parseAllowlist } from "../lib/security-settings";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

const title = "Panel access";
const description = "Limit which network addresses can reach the dashboard and API. Worker enrolment, release downloads, Git webhooks and health checks stay reachable.";
const recovery = "If you lose access, set COVE_PANEL_ALLOWLIST to an empty value in the control plane's environment on the host and restart it. That variable replaces the saved list while it is set.";

function PanelAccessForm({ saved }: { saved: PanelAccess }) {
  const update = useUpdatePanelAccess();
  const online = useOnline();
  const [draft, setDraft] = useState<string>();
  const [confirming, setConfirming] = useState<string[] | null>(null);
  const text = draft ?? saved.allowlist.join("\n");
  const entries = parseAllowlist(text);
  const dirty = draft !== undefined && entries.join("\n") !== saved.allowlist.join("\n");
  const tooMany = entries.length > panelAllowlistLimit;
  const error = tooMany ? `Enter at most ${panelAllowlistLimit} addresses or ranges.` : update.isError ? errorMessage(update.error) : undefined;
  const client = saved.client_address;
  const hint = `IPv4 or IPv6 addresses or CIDR ranges, one per line, up to ${panelAllowlistLimit}. Leave empty to allow every address. The list must include the address you are connecting from.`;

  function edit(value: string) {
    update.reset();
    setDraft(value);
  }

  function save(allowlist: string[]) {
    update.mutate(allowlist, { onSuccess: () => setDraft(undefined), onSettled: () => setConfirming(null) });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!dirty || tooMany) return;
    if (entries.length === 0) save([]);
    else setConfirming(entries);
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title={title}
        description={description}
        width="form"
        footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={!online || tooMany} onDiscard={() => { update.reset(); setDraft(undefined); }} status={update.isSuccess ? "Panel access saved" : undefined} />}
      >
        <fieldset disabled={!online || update.isPending} className="min-w-0 space-y-4">
          {saved.environment_override && (
            <Notice tone="warn" title="The host environment overrides this list">
              COVE_PANEL_ALLOWLIST is set on the control plane host, so the saved list below is not enforced. Remove the variable and restart the control plane to use it.
            </Notice>
          )}
          <SettingRows>
            <SettingRow label="Restriction" description={saved.environment_override ? "Saved list. The host environment decides what is enforced." : undefined}>
              <StatusLabel
                tone={saved.allowlist.length > 0 ? "ok" : "neutral"}
                label={saved.allowlist.length > 0 ? `On, ${saved.allowlist.length} ${saved.allowlist.length === 1 ? "range" : "ranges"} allowed` : "Off, every address allowed"}
              />
            </SettingRow>
            <SettingRow label="Your address" description="As the control plane sees it, after any trusted proxy.">
              {client ? <span className="font-mono text-xs break-all">{client}</span> : <span className="text-xs text-fg-muted">Unknown. Saving a restriction is refused until the control plane can identify your address.</span>}
            </SettingRow>
            <Field id="panel-allowlist" layout="row" label="Allowed addresses" hint={hint} error={error}>
              <div className="space-y-2">
                <textarea
                  id="panel-allowlist"
                  className={`${inputClass} font-mono`}
                  rows={5}
                  spellCheck={false}
                  autoComplete="off"
                  placeholder={"192.0.2.0/24\n2001:db8::/32"}
                  value={text}
                  onChange={(event) => edit(event.target.value)}
                  aria-invalid={error ? true : undefined}
                  aria-describedby={describedBy("panel-allowlist", error, hint)}
                />
                {client && !entries.includes(client) && (
                  <Button size="sm" onClick={() => edit(entries.length > 0 ? `${text.trimEnd()}\n${client}` : client)}>
                    <Plus aria-hidden className="size-3.5" />
                    Add my address
                  </Button>
                )}
              </div>
            </Field>
          </SettingRows>
          <p className="text-xs text-fg-muted">{recovery}</p>
        </fieldset>
      </Section>
      <ConfirmDialog
        open={confirming !== null}
        title="Restrict panel access?"
        confirmLabel="Restrict access"
        pending={update.isPending}
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming) save(confirming);
        }}
      >
        Only {confirming?.length === 1 ? "this address or range" : `these ${confirming?.length ?? 0} addresses or ranges`} will reach the dashboard and API, including for other administrators.
        Requests from anywhere else are refused and audited. {recovery}
      </ConfirmDialog>
    </form>
  );
}

export function PanelAccessSettings() {
  const access = useQuery(panelAccessQuery);
  if (access.isPending) {
    return (
      <Section title={title} width="form">
        <LoadingRows label="Loading panel access" rows={3} />
      </Section>
    );
  }
  if (access.isError) {
    return (
      <Section title={title} width="form">
        <QueryError what="panel access" error={access.error} onRetry={() => void access.refetch()} retrying={access.isFetching} />
      </Section>
    );
  }
  return <PanelAccessForm saved={access.data} />;
}
