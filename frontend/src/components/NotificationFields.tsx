import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { notificationEventsQuery, type Notification } from "../api/connections";
import { groupNotificationEvents, incidentConditions, notificationProviders, providerSpec, selectedIncidentConditions, type NotificationFieldName, type NotificationFormError, type ProviderSpec, type RoutingMode, type EmailRecipient } from "../lib/notifications";
import { choiceClass, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Select } from "./Select";
import { Field, LoadingRows, QueryError, SwitchField } from "./ui";
import { ChoiceGroup } from "./ChoiceGroup";

const legacyChoices = [
  { name: "success", label: "Successful operations" },
  { name: "failure", label: "Failed or cancelled operations" },
  { name: "health", label: "Health alerts and recovery" },
] as const;

function EventCatalogue({ selected }: { selected: string[] }) {
  const catalogue = useQuery(notificationEventsQuery);
  if (catalogue.isPending) return <LoadingRows label="Loading notification events" rows={3} />;
  if (catalogue.isError) return <QueryError what="notification events" error={catalogue.error} onRetry={() => void catalogue.refetch()} retrying={catalogue.isFetching} />;
  if (catalogue.data.length === 0) return <p className="text-xs text-fg-muted">The control plane reported no notification events.</p>;
  return <div className="space-y-4">
    {groupNotificationEvents(catalogue.data).map((group) => <fieldset key={group.key} aria-describedby={group.description ? `notification-events-${group.key}-hint` : undefined}>
      <legend className="text-[13px] font-medium text-fg">{group.title}</legend>
      {group.description && <p id={`notification-events-${group.key}-hint`} className="mt-0.5 text-xs text-fg-muted">{group.description}</p>}
      <div className="mt-2 grid gap-1.5 sm:grid-cols-2">
        {group.events.map((event) => <label key={event.value} className={choiceClass}><Checkbox name="events" value={event.value} defaultChecked={selected.includes(event.value)} /><span className="min-w-0 break-words">{event.label}</span></label>)}
      </div>
    </fieldset>)}
  </div>;
}

function IncidentConditions({ provider, selected }: { provider: string; selected: string[] }) {
  const catalogue = useQuery(notificationEventsQuery);
  if (catalogue.isPending) return <LoadingRows label="Loading health conditions" rows={2} />;
  if (catalogue.isError) return <QueryError what="notification events" error={catalogue.error} onRetry={() => void catalogue.refetch()} retrying={catalogue.isFetching} />;
  const conditions = incidentConditions(catalogue.data);
  if (conditions.length === 0) return <p className="text-xs text-fg-muted">The control plane reported no health conditions with alert and recovery events.</p>;
  return <fieldset aria-describedby="notification-incident-hint">
    <legend className="text-[13px] font-medium text-fg">Health conditions</legend>
    <p id="notification-incident-hint" className="mt-0.5 text-xs text-fg-muted">{providerSpec(provider)?.label ?? provider} receives health alerts only. Each condition opens an incident when a failure is confirmed and resolves it when the condition recovers. One-off security, configuration, import, adoption, rollout, CPU scaling and idle sleep events need another channel.</p>
    <div className="mt-2 grid gap-1.5 sm:grid-cols-2">
      {conditions.map((condition) => <label key={condition.value} className={choiceClass}><Checkbox name="incident_events" value={condition.value} defaultChecked={selected.includes(condition.value)} /><span className="min-w-0 break-words">{condition.label}</span></label>)}
    </div>
  </fieldset>;
}

function EmailRecipientFields({ initial }: { initial?: Notification }) {
  const saved = initial?.provider === "email" ? initial : undefined;
  const [recipient, setRecipient] = useState<EmailRecipient>(saved?.owner ? "owner" : "fixed");
  return <>
    <ChoiceGroup<EmailRecipient> legend="Recipient" name="recipient" value={recipient} onChange={setRecipient} choices={[
      { value: "fixed", label: "Fixed address", description: "Every matching event goes to one email address." },
      { value: "owner", label: "Resource owner", description: "Each event goes to the current owner of the resource it concerns." },
    ]} />
    {recipient === "fixed" ? (
      <Field id="connection-email" label="Recipient email" hint="Sent through this instance's SMTP settings. Tests fail if SMTP is not configured.">
        <input className={inputClass} id="connection-email" name="email" type="email" autoComplete="off" required defaultValue={saved?.email ?? ""} aria-describedby="connection-email-hint" />
      </Field>
    ) : (
      <p className="text-xs text-fg-muted">Sent through this instance's SMTP settings to the owner set on each resource's General page. Delivery is skipped when the resource has no owner, the owner has left the team or has no verified email address, and for node events, which have no resource owner.</p>
    )}
  </>;
}

function describedBy(id: string, error: string | undefined) {
  return error ? `${id}-error` : `${id}-hint`;
}

function Destination({ spec, initial, keeps, error }: { spec: ProviderSpec; initial?: Notification; keeps: boolean; error?: NotificationFormError }) {
  const message = (field: NotificationFieldName) => (error?.field === field ? error.message : undefined);
  const urlError = message("url");
  const tokenError = message("token");
  const targetError = message("target");
  const userKeyError = message("user_key");
  const tokenSaved = keeps && Boolean(initial?.token_set);
  const userKeySaved = keeps && Boolean(initial?.user_key_set);
  return <>
    {spec.url && (
      <Field id="connection-url" label={spec.url.label} optional={keeps} error={urlError} hint={keeps ? `The saved ${spec.url.label.toLowerCase()} is hidden. Leave this blank to keep it.` : spec.url.hint}>
        <input className={inputClass} id="connection-url" name="url" type={spec.url.secret ? "password" : "url"} inputMode="url" autoComplete="off" spellCheck={false} required={!keeps} maxLength={4096} aria-invalid={urlError ? true : undefined} aria-describedby={describedBy("connection-url", urlError)} />
      </Field>
    )}
    {spec.target && (
      <Field id="connection-target" label={spec.target.label} error={targetError} hint={spec.target.hint}>
        <input className={inputClass} id="connection-target" name="target" type="text" autoComplete="off" spellCheck={false} required maxLength={256} defaultValue={keeps ? (initial?.target ?? "") : ""} aria-invalid={targetError ? true : undefined} aria-describedby={describedBy("connection-target", targetError)} />
      </Field>
    )}
    {spec.token && (
      <Field id="connection-token" label={spec.token.label} optional={!spec.token.required || tokenSaved} error={tokenError} hint={tokenSaved ? `The saved ${spec.token.label.toLowerCase()} is hidden. Leave this blank to keep it, or enter a new one to replace it.` : spec.token.hint}>
        <input className={inputClass} id="connection-token" name="token" type="password" autoComplete="new-password" spellCheck={false} required={spec.token.required && !tokenSaved} maxLength={4096} aria-invalid={tokenError ? true : undefined} aria-describedby={describedBy("connection-token", tokenError)} />
      </Field>
    )}
    {spec.userKey && (
      <Field id="connection-user-key" label={spec.userKey.label} optional={userKeySaved} error={userKeyError} hint={userKeySaved ? "The saved user key is hidden. Leave this blank to keep it, or enter a new one to replace it." : spec.userKey.hint}>
        <input className={inputClass} id="connection-user-key" name="user_key" type="password" autoComplete="new-password" spellCheck={false} required={!userKeySaved} maxLength={256} aria-invalid={userKeyError ? true : undefined} aria-describedby={describedBy("connection-user-key", userKeyError)} />
      </Field>
    )}
  </>;
}

export function NotificationFields({ initial, error }: { initial?: Notification; error?: NotificationFormError }) {
  const [provider, setProvider] = useState(initial?.provider ?? "webhook");
  const [routing, setRouting] = useState<RoutingMode>(initial?.events && initial.events.length > 0 ? "events" : "groups");
  const spec = providerSpec(provider);
  const keepsDestination = initial !== undefined && initial.provider === provider;
  const secretSaved = keepsDestination && provider === "webhook" && Boolean(initial.signing_secret_set);
  const eventsError = error?.field === "events" ? error.message : undefined;
  return <>
    <Field id="connection-provider" label="Provider"><Select className={inputClass} id="connection-provider" name="provider" value={provider} onChange={(event) => setProvider(event.target.value)}>{notificationProviders.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</Select></Field>
    {provider === "email" ? (
      <EmailRecipientFields initial={initial} />
    ) : (
      spec && <Destination key={provider} spec={spec} initial={initial} keeps={keepsDestination} error={error} />
    )}
    {provider === "webhook" && (
      <Field id="connection-signing-secret" label="Signing secret" optional hint={secretSaved ? "A signing secret is saved and hidden. Leave this blank to keep it, or enter a new one to replace it." : "32 to 256 characters. Requests then carry X-Cove-Timestamp and an X-Cove-Signature HMAC-SHA256 of the timestamp, a dot and the body."}>
        <input className={inputClass} id="connection-signing-secret" name="signing_secret" type="password" autoComplete="new-password" minLength={32} maxLength={256} aria-describedby="connection-signing-secret-hint" />
      </Field>
    )}
    <SwitchField id="connection-enabled" name="enabled" defaultChecked={initial?.enabled ?? true} label="Enable notifications" />
    {spec?.incident ? (
      <IncidentConditions provider={provider} selected={keepsDestination ? selectedIncidentConditions(initial.events) : []} />
    ) : (
      <>
        <ChoiceGroup<RoutingMode> legend="Events" name="routing" value={routing} onChange={setRouting} choices={[
          { value: "groups", label: "By result", description: "Operation successes, failures and health alerts. Security, configuration, import and adoption events are not included." },
          { value: "events", label: "Specific events", description: "Choose exact operation, health, security, configuration, import and adoption events." },
        ]} />
        {routing === "groups" ? (
          <div className="flex flex-wrap gap-1.5" role="group" aria-label="Result groups">
            {legacyChoices.map((choice) => <label key={choice.name} className={choiceClass}><Checkbox name={choice.name} defaultChecked={initial ? Boolean(initial[choice.name]) : choice.name === "failure"} />{choice.label}</label>)}
          </div>
        ) : (
          <EventCatalogue selected={initial?.events ?? []} />
        )}
      </>
    )}
    {eventsError && <p role="alert" className="text-xs text-bad">{eventsError}</p>}
  </>;
}
