import { useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { usePortBindings, type PortBindingRewrite } from "../api/mutations";
import type { Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Button, Field, Notice, Section } from "./ui";

const defaultAddress = "127.0.0.1";
const title = "Published ports";
const description = "Bind host ports that Compose publishes on every interface to one address instead. Cove rewrites the saved Compose file; containers change only after the next deploy.";

export function PortBindings({ workload, busy }: { workload: Workload; busy: boolean }) {
  const online = useOnline();
  const previewRewrite = usePortBindings(workload.id);
  const saveRewrite = usePortBindings(workload.id);
  const [address, setAddress] = useState(defaultAddress);
  const [preview, setPreview] = useState<{ address: string; result: PortBindingRewrite } | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);

  if (workload.source && !workload.source.inline_compose) {
    return (
      <Section title={title} description={description} width="form">
        <Notice title="Compose comes from the Git repository">
          Change port bindings in <span className="font-mono break-all">{workload.source.compose_path}</span> in the repository, or switch the source to Compose managed in Cove.
        </Notice>
      </Section>
    );
  }

  const target = address.trim() || defaultAddress;
  const previewError = previewRewrite.isError ? errorMessage(previewRewrite.error) : undefined;
  const hint = "An IPv4 or IPv6 address on the node, such as 127.0.0.1 for local access only or a private network address. Bindings that already name an address are kept.";
  const stale = saveRewrite.isError && isStatus(saveRewrite.error, 409);

  function edit(value: string) {
    setAddress(value);
    setPreview(null);
    setAcknowledged(false);
    previewRewrite.reset();
    saveRewrite.reset();
  }

  function runPreview(event?: FormEvent<HTMLFormElement>) {
    event?.preventDefault();
    saveRewrite.reset();
    setAcknowledged(false);
    previewRewrite.mutate({ address: target, confirm: false }, { onSuccess: (result) => setPreview({ address: target, result }) });
  }

  function save() {
    if (!preview) return;
    saveRewrite.mutate(
      { address: preview.address, confirm: true, revision: preview.result.revision },
      {
        onSuccess: () => {
          setPreview(null);
          setAcknowledged(false);
        },
      },
    );
  }

  const changes = preview?.result.changed ?? 0;
  return (
    <Section title={title} description={description} width="form">
      <div className="space-y-4">
        <form onSubmit={runPreview} noValidate className="space-y-3">
          <Field id="port-bind-address" label="Bind address" hint={hint} error={previewError}>
            <input
              id="port-bind-address"
              className={`${inputClass} font-mono`}
              value={address}
              placeholder={defaultAddress}
              autoComplete="off"
              spellCheck={false}
              disabled={previewRewrite.isPending || saveRewrite.isPending}
              onChange={(event) => edit(event.target.value)}
              aria-invalid={previewError ? true : undefined}
              aria-describedby={describedBy("port-bind-address", previewError, hint)}
            />
          </Field>
          <Button type="submit" disabled={!online || saveRewrite.isPending} pending={previewRewrite.isPending}>
            Preview rewrite
          </Button>
        </form>

        {preview && changes === 0 && (
          <Notice tone="success" title="Nothing to rewrite">
            No service publishes a port on every interface. The saved Compose file is unchanged.
          </Notice>
        )}

        {preview && changes > 0 && (
          <div className="space-y-3 border-t border-line pt-4">
            <div>
              <h3 className="text-[13px] font-semibold text-fg">
                {changes} {changes === 1 ? "binding" : "bindings"} will bind to <span className="font-mono break-all">{preview.address}</span>
              </h3>
              <p className="mt-0.5 text-xs text-fg-muted">Review the rewritten Compose file. Cove reformats the YAML when it saves, so comments and spacing may change.</p>
            </div>
            <pre aria-label="Rewritten Compose file" className="max-h-[28rem] overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed text-fg">
              {preview.result.compose}
            </pre>
            <label className="flex items-start gap-2.5 text-[13px] text-fg">
              <Checkbox className="mt-px" checked={acknowledged} disabled={saveRewrite.isPending} onChange={(event) => setAcknowledged(event.target.checked)} />
              <span>
                Save this Compose file. Running containers keep their current ports until I deploy.
              </span>
            </label>
            {busy && <Notice tone="warn" title="An operation is in progress">The rewrite can be saved once the active operation finishes.</Notice>}
            <div className="flex flex-wrap justify-end gap-2">
              <Button variant="ghost" disabled={saveRewrite.isPending} onClick={() => { setPreview(null); setAcknowledged(false); }}>
                Cancel
              </Button>
              <Button variant="primary" disabled={!online || busy || !acknowledged} pending={saveRewrite.isPending} onClick={save}>
                Save rewritten Compose
              </Button>
            </div>
          </div>
        )}

        {saveRewrite.isError && (
          <Notice
            tone="error"
            title="Could not save the rewrite"
            action={stale ? <Button size="sm" disabled={!online} pending={previewRewrite.isPending} onClick={() => runPreview()}>Preview again</Button> : undefined}
          >
            {errorMessage(saveRewrite.error)}
          </Notice>
        )}
        {saveRewrite.isSuccess && (
          <Notice tone="success" title="Compose file saved">
            {saveRewrite.data.changed} {saveRewrite.data.changed === 1 ? "binding now uses" : "bindings now use"} the new address in the saved configuration. Deploy the resource to apply it to the running containers.
          </Notice>
        )}
      </div>
    </Section>
  );
}
