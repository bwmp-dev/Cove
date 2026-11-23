import { useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CloudDownload, Pencil, Plus, RefreshCw, SearchCheck, Trash2, X } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { ApiError, errorMessage } from "../api/client";
import {
  maxProviders,
  maxUnresolvedProvisions,
  nodeCloudQuery,
  nodeProvidersQuery,
  providerCatalogQuery,
  provisionsQuery,
  isUnresolved,
  useAbandonProvision,
  useCreateProvision,
  useDeleteProvider,
  useReconcileProvision,
  useSaveProvider,
  type NodeProvider,
  type ProviderCatalog,
  type ProviderKind,
  type Provision,
  type ProvisionInput,
} from "../api/nodeProviders";
import { useOnline } from "../lib/hooks";
import { architectureMismatch, catalogPrice, catalogPriceInput, optionLabel, parseCost, phaseInfo, planSummary, plansForRegion, providerKinds, providerLabel, validateProvider, validateServerName, type ProviderErrors } from "../lib/node-providers";
import { describedBy, inputClass, listLinkClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Dialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

// Client errors are definite answers; a lost response or a server error may have been applied.
function ambiguous(error: unknown) {
  return !(error instanceof ApiError) || error.status === 0 || error.status >= 500;
}

const billingRule = "The provider bills for each server until it is deleted in the provider's console. Removing the Cove node, abandoning tracking or deleting a saved token never deletes a server.";

function ProviderForm({ provider, onClose }: { provider: NodeProvider | null; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const save = useSaveProvider(provider?.id);
  const [kind, setKind] = useState<ProviderKind>(provider?.kind ?? "hetzner");
  const [name, setName] = useState(provider?.name ?? "");
  const [token, setToken] = useState("");
  const [errors, setErrors] = useState<ProviderErrors>({});
  const editing = provider !== null;
  const hint = editing ? `Leave blank to keep the saved token. ${providerKinds[kind].tokenHint}` : providerKinds[kind].tokenHint;

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const result = validateProvider({ name, token }, editing);
    setErrors(result.errors);
    if (!result.input) return;
    save.mutate({ kind, ...result.input }, {
      onSuccess: () => {
        setToken("");
        onClose();
      },
      onError: () => setToken(""),
    });
  };

  return (
    <form onSubmit={submit} noValidate autoComplete="off" className="space-y-4">
      <fieldset disabled={save.isPending} className="space-y-4">
        <Field id={`${id}-kind`} label="Provider" hint={editing ? "The provider of a saved token cannot be changed." : undefined}>
          <Select id={`${id}-kind`} value={kind} disabled={editing} aria-describedby={editing ? `${id}-kind-hint` : undefined} onChange={(event) => setKind(event.target.value as ProviderKind)}>
            {Object.entries(providerKinds).map(([value, info]) => (
              <option key={value} value={value}>
                {info.label}
              </option>
            ))}
          </Select>
        </Field>
        <Field id={`${id}-name`} label="Name" error={errors.name}>
          <input id={`${id}-name`} className={inputClass} value={name} maxLength={120} onChange={(event) => { setName(event.target.value); save.reset(); }} aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name)} />
        </Field>
        <Field id={`${id}-token`} label={editing ? "Replacement API token" : "API token"} optional={editing} error={errors.token} hint={hint}>
          <input
            id={`${id}-token`}
            type="password"
            className={`${inputClass} font-mono`}
            value={token}
            maxLength={4096}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            onChange={(event) => { setToken(event.target.value); save.reset(); }}
            aria-invalid={errors.token ? true : undefined}
            aria-describedby={describedBy(`${id}-token`, errors.token, hint)}
          />
        </Field>
      </fieldset>
      <p className="text-xs text-fg-muted">Cove seals the token on the panel and never shows it again. Anyone with this token can create and delete servers in your provider account, so revoke it in the provider console if it leaks.</p>
      {!online && <Notice tone="warn">You are offline. Reconnect to save the token.</Notice>}
      {save.isError && (
        <Notice tone="error" title="Could not save the provider token">
          <p>{errorMessage(save.error)}</p>
          {ambiguous(save.error) && <p className="mt-1">The saved tokens were reloaded; check the list before trying again.</p>}
        </Notice>
      )}
      <div className="flex justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" pending={save.isPending} disabled={!online}>
          {editing ? "Save changes" : "Save token"}
        </Button>
      </div>
    </form>
  );
}

function DeleteProviderDialog({ provider, onClose }: { provider: NodeProvider | null; onClose: () => void }) {
  const remove = useDeleteProvider();
  const online = useOnline();
  const close = () => {
    remove.reset();
    onClose();
  };
  return (
    <Dialog
      open={provider !== null}
      title={`Delete ${provider?.name ?? "provider token"}?`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close} disabled={remove.isPending}>
            Cancel
          </Button>
          <Button variant="danger" pending={remove.isPending} disabled={!online} onClick={() => provider && remove.mutate(provider.id, { onSuccess: close })}>
            Delete token
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p>Cove deletes the saved token only. Servers already created with it keep running and stay billed by the provider; delete them in the provider console. Nodes are not removed, and the token stays valid at the provider until you revoke it there.</p>
        <p>A token with unfinished server creations cannot be deleted. Wait for them to finish or abandon tracking first.</p>
        {!online && <Notice tone="warn">You are offline. Reconnect to delete the token.</Notice>}
        {remove.isError && (
          <Notice tone="error" title="Could not delete the provider token">
            {errorMessage(remove.error)}
          </Notice>
        )}
      </div>
    </Dialog>
  );
}

export function ProviderTokens() {
  const providers = useQuery(nodeProvidersQuery);
  const online = useOnline();
  const [editing, setEditing] = useState<NodeProvider | "new" | null>(null);
  const [deleting, setDeleting] = useState<NodeProvider | null>(null);
  const full = (providers.data?.length ?? 0) >= maxProviders;
  const current = editing === "new" ? null : editing;

  return (
    <Section
      title="Cloud provider tokens"
      description="API tokens Cove uses to list a provider's regions, plans and images and to create servers that enroll as nodes. Tokens are write-only and never shown again."
      flush
      actions={
        <Button onClick={() => setEditing("new")} disabled={!providers.data || full || !online} title={full ? `At most ${maxProviders} provider tokens are supported` : undefined}>
          <Plus aria-hidden className="size-3.5" />
          Add token
        </Button>
      }
    >
      {providers.isPending ? (
        <LoadingRows label="Loading provider tokens" rows={2} />
      ) : providers.isError && !providers.data ? (
        <div className="p-3">
          <QueryError what="provider tokens" error={providers.error} onRetry={() => void providers.refetch()} retrying={providers.isFetching} />
        </div>
      ) : (
        <>
          {full && (
            <div className="border-b border-line px-4 py-3">
              <Notice>This instance has the maximum of {maxProviders} provider tokens. Delete one to add another.</Notice>
            </div>
          )}
          {providers.data.length === 0 ? (
            <p className="px-4 py-4 text-[13px] text-fg-muted">No provider tokens. Add a Hetzner Cloud, DigitalOcean or Vultr token to create servers from Cove. Adding nodes manually does not need one.</p>
          ) : (
            <ul className="divide-y divide-line" aria-label="Cloud provider tokens">
              {providers.data.map((provider) => (
                <li key={provider.id} className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2.5 hover:bg-raised">
                  <div className="min-w-0 flex-1 basis-48">
                    <p className="text-[13px] font-medium break-words text-fg">{provider.name}</p>
                    <p className="text-xs text-fg-muted">
                      {providerLabel(provider.kind)} Â· {provider.has_token ? "Token saved" : "No token saved"}
                    </p>
                  </div>
                  <div className="flex gap-1">
                    <Button variant="ghost" size="sm" onClick={() => setEditing(provider)} disabled={!online} aria-label={`Edit ${provider.name}`}>
                      <Pencil aria-hidden className="size-3.5" />
                      Edit
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => setDeleting(provider)} disabled={!online} aria-label={`Delete ${provider.name}`}>
                      <Trash2 aria-hidden className="size-3.5" />
                      Delete
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      <Dialog open={editing !== null} title={current ? `Edit ${current.name}` : "Add provider token"} onClose={() => setEditing(null)}>
        <ProviderForm key={current?.id ?? "new"} provider={current} onClose={() => setEditing(null)} />
      </Dialog>
      <DeleteProviderDialog provider={deleting} onClose={() => setDeleting(null)} />
    </Section>
  );
}

function CatalogStatus({ catalog }: { catalog: ProviderCatalog }) {
  const empty = [catalog.regions.length === 0 && "regions", catalog.plans.length === 0 && "plans", catalog.images.length === 0 && "supported Linux images"].filter(Boolean);
  return (
    <>
      {empty.length > 0 && (
        <Notice tone="warn" title="Incomplete catalogue">
          The provider returned no {empty.join(", ")} for this token. A server cannot be created until it does. Check the token's project and permissions in the provider console.
        </Notice>
      )}
      {catalog.limited && <Notice>The provider returned more choices than Cove lists. Only the first 512 of each kind are shown.</Notice>}
    </>
  );
}

interface CreateDraft {
  provider: NodeProvider;
  input: ProvisionInput;
  region: string;
  plan: string;
  planPrice: string;
  image: string;
}

function CreateConfirmDialog({ draft, onCancel, onCreated }: { draft: CreateDraft | null; onCancel: () => void; onCreated: () => void }) {
  const create = useCreateProvision();
  const online = useOnline();
  const [acknowledged, setAcknowledged] = useState(false);
  const checkId = useId();
  const close = () => {
    setAcknowledged(false);
    create.reset();
    onCancel();
  };
  const confirm = () => {
    if (!draft) return;
    create.mutate(draft.input, {
      onSuccess: () => {
        setAcknowledged(false);
        create.reset();
        onCreated();
      },
      onError: () => setAcknowledged(false),
    });
  };
  return (
    <Dialog
      open={draft !== null}
      title="Create a paid server?"
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close} disabled={create.isPending}>
            Cancel
          </Button>
          <Button variant="primary" pending={create.isPending} disabled={!acknowledged || !online} onClick={confirm}>
            Create paid server
          </Button>
        </>
      }
    >
      {draft && (
        <div className="space-y-3">
          <DefinitionList
            compact
            items={[
              { term: "Provider", value: `${draft.provider.name} (${providerLabel(draft.provider.kind)})` },
              { term: "Server name", value: <span className="break-all">{draft.input.name}</span> },
              { term: "Region", value: draft.region },
              { term: "Plan", value: draft.plan },
              { term: "Estimate", value: draft.planPrice },
              { term: "Image", value: draft.image },
            ]}
          />
          <ul className="list-disc space-y-1 pl-5">
            <li>The provider bills for this server from creation until you delete it in the provider console. The estimate excludes taxes, traffic, IPv4 addresses and other fees; check the price there before you continue.</li>
            <li>Removing the node from Cove or abandoning tracking does not delete the server.</li>
            <li>A failed or uncertain creation can still leave a billable server. Cove never repeats a creation automatically.</li>
          </ul>
          <label htmlFor={checkId} className="flex items-start gap-2.5 text-fg">
            <Checkbox id={checkId} checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} disabled={create.isPending} className="mt-0.5" />
            <span>I understand that this creates a paid server that I must delete in the provider console.</span>
          </label>
          {!online && <Notice tone="warn">You are offline. Reconnect to create the server.</Notice>}
          {create.isError && (
            <Notice tone={ambiguous(create.error) ? "warn" : "error"} title={ambiguous(create.error) ? "The creation may have been queued" : "Could not queue the server"}>
              <p>{errorMessage(create.error)}</p>
              {ambiguous(create.error) && <p className="mt-1">Cove did not confirm the result. Check the job list after closing this dialog before trying again; another attempt can create a second paid server.</p>}
            </Notice>
          )}
        </div>
      )}
    </Dialog>
  );
}

interface Choice {
  region: string;
  plan: string;
  image: string;
}

// Saved choices that the current catalogue no longer lists resolve to nothing instead of being sent.
function resolveChoice(data: ProviderCatalog | undefined, choice: Choice) {
  const region = data?.regions.find((option) => option.id === choice.region);
  const plans = data ? plansForRegion(data.plans, region?.id ?? "") : [];
  return { region, plans, plan: plans.find((option) => option.id === choice.plan), image: data?.images.find((option) => option.id === choice.image) };
}

function CatalogChoices({ id, data, choice, onChange }: { id: string; data: ProviderCatalog; choice: Choice; onChange: (next: Partial<Choice>) => void }) {
  const { region, plans, plan, image } = resolveChoice(data, choice);
  const planHint = plan
    ? "The provider still decides availability when the server is created."
    : region
      ? `${plans.length.toLocaleString("en")} of ${data.plans.length.toLocaleString("en")} plans list ${region.name}. The provider still decides availability when the server is created.`
      : "Choose a region to list the plans available there.";
  return (
    <>
      <CatalogStatus catalog={data} />
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-region`} label="Region">
          <Select id={`${id}-region`} value={region?.id ?? ""} disabled={data.regions.length === 0} onChange={(event) => onChange({ region: event.target.value })}>
            <option value="">Choose a region</option>
            {data.regions.map((option) => (
              <option key={option.id} value={option.id}>
                {option.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field id={`${id}-image`} label="Image">
          <Select id={`${id}-image`} value={image?.id ?? ""} disabled={data.images.length === 0} onChange={(event) => onChange({ image: event.target.value })}>
            <option value="">Choose an image</option>
            {data.images.map((option) => (
              <option key={option.id} value={option.id}>
                {optionLabel(option)}
              </option>
            ))}
          </Select>
        </Field>
        <div className="sm:col-span-2">
          <Field id={`${id}-plan`} label="Plan" hint={planHint}>
            <Select id={`${id}-plan`} value={plan?.id ?? ""} disabled={plans.length === 0} aria-describedby={`${id}-plan-hint`} onChange={(event) => onChange({ plan: event.target.value })}>
              <option value="">Choose a plan</option>
              {plans.map((option) => (
                <option key={option.id} value={option.id}>
                  {`${option.name} Â· ${planSummary(option)}`}
                </option>
              ))}
            </Select>
          </Field>
        </div>
      </div>
      {architectureMismatch(plan, image) && <Notice tone="warn">The plan is {plan?.architecture} but the image is {image?.architecture}. Choose a matching image or the provider will refuse the server.</Notice>}
    </>
  );
}

function CatalogHeader({ data, fetching, disabled, onLoad }: { data: ProviderCatalog | undefined; fetching: boolean; disabled: boolean; onLoad: () => void }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2">
      <div className="min-w-0">
        <p className="text-[13px] font-medium text-fg">Catalogue</p>
        <p className="text-xs text-fg-muted">
          {data ? (
            <>
              Loaded from the provider <RelativeTime value={data.checked_at} />. Prices are the provider&apos;s estimates.
            </>
          ) : (
            "Regions, plans and images are read live from the provider with the saved token."
          )}
        </p>
      </div>
      <Button size="sm" onClick={onLoad} pending={fetching} disabled={disabled}>
        {!fetching && (data ? <RefreshCw aria-hidden className="size-3.5" /> : <CloudDownload aria-hidden className="size-3.5" />)}
        {data ? "Reload catalogue" : "Load catalogue"}
      </Button>
    </div>
  );
}

interface PriceValues {
  currency: string;
  amount: string;
}

function NodePriceFields({ id, values, errors, suggested, onChange }: { id: string; values: PriceValues; errors: { currency?: string; amount?: string }; suggested: PriceValues | undefined; onChange: (next: Partial<PriceValues>) => void }) {
  return (
    <div className="space-y-3 border-t border-line pt-4">
      <div>
        <p className="text-[13px] font-medium text-fg">
          Node price <span className="font-normal text-fg-subtle">Optional</span>
        </p>
        <p className="text-xs text-fg-muted">Recorded as the node&apos;s monthly price for project cost estimates. It is your own estimate; Cove does not read invoices.</p>
      </div>
      <div className="grid gap-4 sm:grid-cols-[8rem_minmax(0,1fr)_auto] sm:items-end">
        <Field id={`${id}-currency`} label="Currency" error={errors.currency}>
          <input id={`${id}-currency`} className={`${inputClass} font-mono uppercase`} value={values.currency} maxLength={3} placeholder="USD" autoComplete="off" spellCheck={false} onChange={(event) => onChange({ currency: event.target.value })} aria-invalid={errors.currency ? true : undefined} aria-describedby={describedBy(`${id}-currency`, errors.currency)} />
        </Field>
        <Field id={`${id}-amount`} label="Monthly price" error={errors.amount}>
          <input id={`${id}-amount`} className={`${inputClass} tabular-nums`} value={values.amount} inputMode="decimal" placeholder="0.00" autoComplete="off" onChange={(event) => onChange({ amount: event.target.value })} aria-invalid={errors.amount ? true : undefined} aria-describedby={describedBy(`${id}-amount`, errors.amount)} />
        </Field>
        {suggested && (
          <Button variant="ghost" onClick={() => onChange(suggested)}>
            Use plan estimate
          </Button>
        )}
      </div>
    </div>
  );
}

function choiceProblem(provider: NodeProvider | undefined, data: ProviderCatalog | undefined, resolved: ReturnType<typeof resolveChoice>) {
  if (!provider) return "Choose a provider token.";
  if (!data) return "Load the provider's catalogue first.";
  if (!resolved.region || !resolved.plan || !resolved.image) return "Choose a region, plan and image.";
  return undefined;
}

const emptyChoice: Choice = { region: "", plan: "", image: "" };

function CreateServerForm({ providers, onClose }: { providers: NodeProvider[]; onClose: (queued: boolean) => void }) {
  const id = useId();
  const online = useOnline();
  const [providerId, setProviderId] = useState(providers.length === 1 ? (providers[0]?.id ?? "") : "");
  const [name, setName] = useState("");
  const [choice, setChoice] = useState(emptyChoice);
  const [price, setPrice] = useState<PriceValues>({ currency: "", amount: "" });
  const [errors, setErrors] = useState<{ name?: string; choice?: string; currency?: string; amount?: string }>({});
  const [draft, setDraft] = useState<CreateDraft | null>(null);
  const catalog = useQuery(providerCatalogQuery(providerId));
  const provider = providers.find((item) => item.id === providerId);
  const data = providerId ? catalog.data : undefined;
  const resolved = resolveChoice(data, choice);
  const suggested = resolved.plan ? catalogPriceInput(resolved.plan) : undefined;

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const nameError = validateServerName(name);
    const problem = choiceProblem(provider, data, resolved);
    const { cost, errors: costErrors } = parseCost(price.currency, price.amount);
    setErrors({ name: nameError, choice: problem, ...costErrors });
    const { region, plan, image } = resolved;
    if (nameError || problem || costErrors.currency || costErrors.amount || !provider || !region || !plan || !image) return;
    setDraft({
      provider,
      input: { provider_id: provider.id, name: name.trim(), region: region.id, plan: plan.id, image: image.id, ...(cost ? { cost } : {}) },
      region: region.name,
      plan: `${plan.name} Â· ${planSummary(plan)}`,
      planPrice: catalogPrice(plan) ?? "Unknown; check the provider console",
      image: optionLabel(image),
    });
  };

  return (
    <Subsection title="Create a cloud server" description="Cove asks the provider for one server, installs the worker from the server's first-boot script, pins its SSH host key and adds it as a node for the current team.">
      <form onSubmit={submit} noValidate className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-provider`} label="Provider token">
            <Select
              id={`${id}-provider`}
              value={providerId}
              onChange={(event) => {
                setProviderId(event.target.value);
                setChoice(emptyChoice);
              }}
            >
              <option value="">Choose a provider token</option>
              {providers.map((item) => (
                <option key={item.id} value={item.id}>
                  {`${item.name} (${providerLabel(item.kind)})`}
                </option>
              ))}
            </Select>
          </Field>
          <Field id={`${id}-name`} label="Server name" error={errors.name} hint="Also the node's name. Use a name the provider accepts as a hostname.">
            <input id={`${id}-name`} className={`${inputClass} font-mono`} value={name} maxLength={120} autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder="worker-1" onChange={(event) => setName(event.target.value)} aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name, true)} />
          </Field>
        </div>

        <div className="space-y-3 border-t border-line pt-4">
          <CatalogHeader data={data} fetching={catalog.isFetching} disabled={!providerId || !online} onLoad={() => void catalog.refetch()} />
          {providerId && catalog.isError && <QueryError what="the provider's catalogue" error={catalog.error} onRetry={() => void catalog.refetch()} retrying={catalog.isFetching} />}
          {data && <CatalogChoices id={id} data={data} choice={choice} onChange={(next) => setChoice((current) => ({ ...current, ...next }))} />}
          {errors.choice && <p className="text-xs text-bad">{errors.choice}</p>}
        </div>

        <NodePriceFields id={id} values={price} errors={errors} suggested={suggested} onChange={(next) => setPrice((current) => ({ ...current, ...next }))} />

        <Notice title="Before you create a server">
          The new server must reach this panel&apos;s public HTTPS address and download packages, and must accept SSH on port 22 from the main node or its bastion. The worker installer supports the same Linux distributions as manual setup. If the first-boot setup command expires before the server enrolls, renew it on the node&apos;s page.
        </Notice>
        {!online && <Notice tone="warn">You are offline. Reconnect to create a server.</Notice>}
        <div className="flex justify-end gap-2 border-t border-line pt-3">
          <Button variant="ghost" onClick={() => onClose(false)}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!online}>
            Review paid server
          </Button>
        </div>
      </form>
      <CreateConfirmDialog draft={draft} onCancel={() => setDraft(null)} onCreated={() => onClose(true)} />
    </Subsection>
  );
}

function ReconcileDialog({ job, onClose }: { job: Provision | null; onClose: () => void }) {
  const reconcile = useReconcileProvision();
  const online = useOnline();
  const id = useId();
  const [serverId, setServerId] = useState("");
  const known = job?.server_id ?? "";
  const close = () => {
    setServerId("");
    reconcile.reset();
    onClose();
  };
  const hint = known ? "The provider already acknowledged this server ID; it cannot be changed." : "If you found the server in the provider console, enter its exact ID. Cove only accepts a server it created for this job.";
  return (
    <Dialog
      open={job !== null}
      title={`Reconcile ${job?.server_name ?? "server"}?`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close} disabled={reconcile.isPending}>
            Cancel
          </Button>
          <Button variant="primary" pending={reconcile.isPending} disabled={!online} onClick={() => job && reconcile.mutate({ id: job.id, serverId: known ? undefined : serverId.trim() || undefined }, { onSuccess: close })}>
            Look up server
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p>Cove asks the provider for a server with this job&apos;s marker and exact name and, if one exists, continues enrolling it. Reconciling never creates another server. If nothing is found the job stays uncertain; a server may still appear later and is billed while it exists.</p>
        <Field id={`${id}-server`} label="Provider server ID" optional={!known} hint={hint}>
          <input id={`${id}-server`} className={`${inputClass} font-mono`} value={known || serverId} readOnly={Boolean(known)} disabled={reconcile.isPending} maxLength={128} autoComplete="off" spellCheck={false} onChange={(event) => setServerId(event.target.value)} aria-describedby={`${id}-server-hint`} />
        </Field>
        {!online && <Notice tone="warn">You are offline. Reconnect to reconcile.</Notice>}
        {reconcile.isError && (
          <Notice tone="error" title="Could not reconcile">
            {errorMessage(reconcile.error)}
          </Notice>
        )}
      </div>
    </Dialog>
  );
}

function AbandonDialog({ job, onClose }: { job: Provision | null; onClose: () => void }) {
  const abandon = useAbandonProvision();
  const online = useOnline();
  const close = () => {
    abandon.reset();
    onClose();
  };
  return (
    <Dialog
      open={job !== null}
      title={`Abandon tracking ${job?.server_name ?? "server"}?`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close} disabled={abandon.isPending}>
            Keep tracking
          </Button>
          <Button variant="danger" pending={abandon.isPending} disabled={!online} onClick={() => job && abandon.mutate(job.id, { onSuccess: close })}>
            Abandon tracking
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p>Cove stops tracking this job and revokes its unused setup command. It does not delete or cancel anything at the provider.</p>
        <Notice tone="warn" title="The server may still be billed">
          The provider may already have created the server, or may still create it. Check the provider console{job?.server_id ? ` for server ${job.server_id}` : ""} and delete it there if you do not need it.
        </Notice>
        {!online && <Notice tone="warn">You are offline. Reconnect to abandon tracking.</Notice>}
        {abandon.isError && (
          <Notice tone="error" title="Could not abandon tracking">
            {errorMessage(abandon.error)}
          </Notice>
        )}
      </div>
    </Dialog>
  );
}

function ProvisionRow({ job, online, onReconcile, onAbandon }: { job: Provision; online: boolean; onReconcile: () => void; onAbandon: () => void }) {
  const info = phaseInfo(job.phase);
  const unresolved = isUnresolved(job.phase);
  const active = unresolved && job.phase !== "uncertain";
  return (
    <li className="flex flex-wrap items-start gap-x-4 gap-y-2 px-4 py-3 hover:bg-raised md:grid md:grid-cols-[minmax(0,2fr)_minmax(0,3fr)_13rem] md:gap-x-6">
      <div className="min-w-0 flex-1 basis-64 space-y-0.5">
        <p className="font-mono text-[13px] font-medium break-all text-fg">{job.server_name}</p>
        <p className="text-xs break-words text-fg-muted">
          {providerLabel(job.provider_kind)} Â· {job.provider_id ? job.provider_name : `${job.provider_name || "Deleted token"} (token removed)`}
        </p>
        {job.server_id && (
          <p className="text-xs text-fg-muted">
            Server ID <span className="font-mono break-all text-fg">{job.server_id}</span>
          </p>
        )}
        <p className="text-xs text-fg-subtle">
          Started <RelativeTime value={job.created_at} />
          {active ? (
            <>
              {" "}
              Â· Deadline <RelativeTime value={job.deadline} />
            </>
          ) : (
            <>
              {" "}
              Â· Updated <RelativeTime value={job.updated_at} />
            </>
          )}
        </p>
      </div>
      <div className="min-w-0 flex-1 basis-64 space-y-1">
        <StatusLabel tone={info.tone} label={info.label} pulse={active} />
        <p className="text-xs text-fg-muted">{info.description}</p>
        {job.last_error && <p className="text-xs break-words text-bad">{job.last_error}</p>}
        {job.node_id && (
          <Link to="/nodes/$nodeId" params={{ nodeId: job.node_id }} className={`${listLinkClass} text-xs`}>
            Open node
          </Link>
        )}
      </div>
      {unresolved ? (
        <div className="flex gap-1 md:justify-end">
          {job.phase === "uncertain" && (
            <Button variant="ghost" size="sm" onClick={onReconcile} disabled={!online} aria-label={`Reconcile ${job.server_name}`}>
              <SearchCheck aria-hidden className="size-3.5" />
              Reconcile
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={onAbandon} disabled={!online} aria-label={`Abandon tracking ${job.server_name}`}>
            <X aria-hidden className="size-3.5" />
            Abandon tracking
          </Button>
        </div>
      ) : (
        <span aria-hidden className="hidden md:block" />
      )}
    </li>
  );
}

function unresolvedCount(jobs: Provision[] | undefined) {
  return jobs?.reduce((count, job) => (isUnresolved(job.phase) ? count + 1 : count), 0) ?? 0;
}

function createBlocked(noProviders: boolean, atLimit: boolean) {
  if (noProviders) return "Add a provider token first";
  if (atLimit) return `At most ${maxUnresolvedProvisions} creations can be unfinished at once`;
  return undefined;
}

function ProvisionList({ jobs, online }: { jobs: UseQueryResult<Provision[]>; online: boolean }) {
  const [reconciling, setReconciling] = useState<Provision | null>(null);
  const [abandoning, setAbandoning] = useState<Provision | null>(null);
  if (jobs.isPending) return <LoadingRows label="Loading cloud servers" rows={2} />;
  if (jobs.isError && !jobs.data) {
    return (
      <div className="p-3">
        <QueryError what="cloud server jobs" error={jobs.error} onRetry={() => void jobs.refetch()} retrying={jobs.isFetching} />
      </div>
    );
  }
  return (
    <div>
      {jobs.isRefetchError && (
        <div className="px-4 py-3">
          <QueryError what="the latest job state" error={jobs.error} onRetry={() => void jobs.refetch()} retrying={jobs.isFetching} />
        </div>
      )}
      {jobs.data.length === 0 ? (
        <p className="px-4 py-4 text-[13px] text-fg-muted">No cloud servers created yet.</p>
      ) : (
        <ul className="divide-y divide-line" aria-label="Cloud server jobs">
          {jobs.data.map((job) => (
            <ProvisionRow key={job.id} job={job} online={online} onReconcile={() => setReconciling(job)} onAbandon={() => setAbandoning(job)} />
          ))}
        </ul>
      )}
      <ReconcileDialog job={reconciling} onClose={() => setReconciling(null)} />
      <AbandonDialog job={abandoning} onClose={() => setAbandoning(null)} />
    </div>
  );
}

export function CloudServers() {
  const client = useQueryClient();
  const online = useOnline();
  const providers = useQuery(nodeProvidersQuery);
  const jobs = useQuery(provisionsQuery(client));
  const [creating, setCreating] = useState(false);
  const [queued, setQueued] = useState(false);
  const atLimit = unresolvedCount(jobs.data) >= maxUnresolvedProvisions;
  const noProviders = providers.data?.length === 0;
  const canCreate = Boolean(providers.data && jobs.data) && !noProviders && !atLimit && online;
  const reason = createBlocked(noProviders, atLimit);

  return (
    <Section
      title="Cloud servers"
      description="Servers Cove created through a saved provider token, newest first. Finished jobs are kept for 30 days."
      flush
      actions={
        !creating && (
          <Button
            onClick={() => {
              setQueued(false);
              setCreating(true);
            }}
            disabled={!canCreate}
            title={reason}
          >
            <Plus aria-hidden className="size-3.5" />
            Create server
          </Button>
        )
      }
    >
      <div className="divide-y divide-line">
        <div className="space-y-3 px-4 py-3">
          <Notice tone="warn" title="Billing happens at the provider">
            {billingRule}
          </Notice>
          {atLimit && <Notice>{maxUnresolvedProvisions} creations are unfinished. Wait for one to finish, reconcile it or abandon tracking before creating another server.</Notice>}
          {queued && (
            <Notice tone="success" title="Server creation queued">
              Follow its progress below. Cove does not repeat the creation if it fails.
            </Notice>
          )}
        </div>
        {creating && providers.data && (
          <CreateServerForm
            providers={providers.data}
            onClose={(wasQueued) => {
              setCreating(false);
              setQueued(wasQueued);
            }}
          />
        )}
        <ProvisionList jobs={jobs} online={online} />
      </div>
    </Section>
  );
}

// Shown on a node's page to instance administrators. Manually added nodes have no cloud origin.
export function NodeCloudOrigin({ nodeId }: { nodeId: string }) {
  const origin = useQuery(nodeCloudQuery(nodeId));
  if (origin.isPending || (origin.data && origin.data.kind === "")) return null;
  return (
    <Section title="Cloud server" description="This node was created through a saved provider token.">
      {origin.isError ? (
        <QueryError what="the node's cloud origin" error={origin.error} onRetry={() => void origin.refetch()} retrying={origin.isFetching} />
      ) : (
        <div className="space-y-4">
          <Notice tone="warn" title="Deleting this node does not delete the server">
            The provider bills for the server until you delete it in the provider console. Removing the node from Cove leaves the server running.
          </Notice>
          <DefinitionList
            items={[
              { term: "Provider", value: `${providerLabel(origin.data.kind)}${origin.data.provider_id ? "" : " (token removed)"}` },
              { term: "Server name", value: <span className="font-mono break-all">{origin.data.server_name}</span> },
              { term: "Server ID", value: origin.data.server_id ? <span className="font-mono break-all">{origin.data.server_id}</span> : <span className="text-fg-subtle">Not reported</span> },
              { term: "Recorded", value: <RelativeTime value={origin.data.checked_at} /> },
            ]}
          />
        </div>
      )}
    </Section>
  );
}
