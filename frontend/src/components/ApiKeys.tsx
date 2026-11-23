import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { accessTokenScopesQuery, accessTokensQuery, useCreateAccessToken, useRevokeAccessToken, type AccessToken, type ApiScope, type ApiScopeLevel } from "../api/access-tokens";
import { errorMessage } from "../api/client";
import { formatAbsolute } from "../lib/format";
import { useNow, useOnline } from "../lib/hooks";
import { currentTeam, managesTeam, permissionLabel } from "../lib/permissions";
import { useSession } from "../lib/session";
import { choiceClass, inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError } from "./ui";
import { ChoiceGroup } from "./WorkloadForm";

const levelLabels: Record<ApiScopeLevel, { title: string; description: string }> = {
  1: { title: "Read", description: "Needs viewer access." },
  2: { title: "Operate", description: "Needs operator access." },
  3: { title: "Change", description: "Needs the access listed on each permission." },
};

const scopeLabels: Record<AccessToken["scope"], string> = { user: "Personal", team: "Team" };

function useCanManageTeamKeys() {
  const session = useSession();
  return Boolean(session?.is_admin) || managesTeam(currentTeam(session));
}

// The server refuses these scopes for other accounts or owners; the reason is shown on the disabled choice.
function unavailableReason(scope: ApiScope, admin: boolean, owner: AccessToken["scope"]) {
  if (scope.instance_admin && !admin) return "Only instance administrators can grant this permission.";
  if (scope.personal_only && owner !== "user") return "Available on personal keys only.";
  return undefined;
}

// View is part of every role, so only the permissions that narrow access are named.
function requirement(scope: ApiScope) {
  const needed = scope.permissions.filter((permission) => permission !== "view").map(permissionLabel);
  if (needed.length === 0) return "";
  return `Requires ${needed.length === 1 ? needed[0] : `${needed.slice(0, -1).join(", ")} and ${needed.at(-1)}`}.`;
}

function availableSelection(selected: ReadonlySet<string>, scopes: ApiScope[] | undefined, admin: boolean, owner: AccessToken["scope"]) {
  const allowed = new Set(scopes?.filter((scope) => unavailableReason(scope, admin, owner) === undefined).map((scope) => scope.name));
  return new Set([...selected].filter((name) => allowed.has(name)));
}

interface ScopeChoicesProps {
  scopes: ApiScope[];
  selected: ReadonlySet<string>;
  onToggle: (name: string) => void;
  unavailable: (scope: ApiScope) => string | undefined;
}

function ScopeChoices({ scopes, selected, onToggle, unavailable }: ScopeChoicesProps) {
  return (
    <fieldset className="space-y-3">
      <legend className="text-[13px] font-medium text-fg">Permissions</legend>
      <p id="key-scopes-hint" className="text-xs text-fg-muted">
        Choose at least one. Every request also rechecks the creator's current access, so a key never does more than its creator can.
      </p>
      {([1, 2, 3] as const).map((level) => {
        const group = scopes.filter((scope) => scope.level === level);
        if (group.length === 0) return null;
        return (
          <div key={level} className="space-y-1.5">
            <p className="text-[11px] font-medium text-fg-subtle">
              {levelLabels[level].title} · {levelLabels[level].description}
            </p>
            <div className="grid gap-1.5">
              {group.map((scope) => {
                const reason = unavailable(scope);
                return (
                  <label key={scope.name} className={`${choiceClass} items-start`}>
                    <Checkbox
                      className="mt-0.5"
                      checked={selected.has(scope.name)}
                      disabled={reason !== undefined}
                      onChange={() => onToggle(scope.name)}
                      aria-describedby={reason ? `scope-${scope.name}-description scope-${scope.name}-reason` : `scope-${scope.name}-description`}
                    />
                    <span className="min-w-0 font-mono text-xs text-fg">
                      {scope.name}
                      <span id={`scope-${scope.name}-description`} className="block font-sans text-fg-muted">
                        {scope.description}
                        {requirement(scope) && ` ${requirement(scope)}`}
                      </span>
                      {reason && (
                        <span id={`scope-${scope.name}-reason`} className="block font-sans text-fg-muted">
                          {reason}
                        </span>
                      )}
                    </span>
                  </label>
                );
              })}
            </div>
          </div>
        );
      })}
    </fieldset>
  );
}

function IssuedKey({ token, expiresAt, onDone }: { token: string; expiresAt: string; onDone: () => void }) {
  const field = useRef<HTMLInputElement>(null);
  // The form that held focus is replaced by the one-time key, so move focus to it once.
  useEffect(() => field.current?.focus(), []);
  return (
    <div className="min-w-0 space-y-3">
      <Notice title="Copy this API key now">It will not be shown again. It expires {formatAbsolute(expiresAt)}.</Notice>
      <Field id="issued-api-key" label="New API key">
        <input id="issued-api-key" ref={field} className={`${inputClass} font-mono`} readOnly type="password" value={token} />
      </Field>
      <div className="flex flex-wrap gap-2">
        <CopyButton text={token} label="Copy key" />
        <Button onClick={onDone}>Done</Button>
      </div>
    </div>
  );
}

export function CreateApiKey() {
  const session = useSession();
  const online = useOnline();
  const teamAdmin = useCanManageTeamKeys();
  const scopes = useQuery(accessTokenScopesQuery);
  const create = useCreateAccessToken();
  const [name, setName] = useState("");
  const [scope, setScope] = useState<AccessToken["scope"]>("user");
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set());
  const [days, setDays] = useState(30);
  const [submitted, setSubmitted] = useState(false);
  const team = currentTeam(session);
  const admin = session?.is_admin === true;
  const owner = teamAdmin ? scope : "user";
  const unavailable = (item: ApiScope) => unavailableReason(item, admin, owner);
  // Verified legacy accounts can still carry the bootstrap ID; the backend refuses only when no individual email is known.
  const bootstrap = session?.user_id === "bootstrap" && !session.email;

  if (create.data) {
    return (
      <IssuedKey
        token={create.data.token}
        expiresAt={create.data.expires_at}
        onDone={() => {
          create.reset();
          setName("");
          setSelected(new Set());
          setSubmitted(false);
        }}
      />
    );
  }

  const nameError = name.trim() === "" ? "Enter a name." : undefined;
  const daysError = !Number.isInteger(days) || days < 1 || days > 365 ? "Enter a whole number of days from 1 to 365." : undefined;
  const scopesError = selected.size === 0 ? "Choose at least one permission." : undefined;

  function toggle(name: string) {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }

  return (
    <form
      className="min-w-0 space-y-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        setSubmitted(true);
        if (nameError || daysError || scopesError) return;
        create.mutate({ name: name.trim(), scope: owner, scopes: scopes.data?.filter((item) => selected.has(item.name)).map((item) => item.name) ?? [], days });
      }}
    >
      {bootstrap && (
        <Notice tone="warn" title="Sign in with your own account">
          API keys belong to a person. The shared administrator sign-in cannot create them.
        </Notice>
      )}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="key-name" label="Name" error={submitted ? nameError : undefined}>
          <input id="key-name" className={inputClass} maxLength={120} value={name} aria-invalid={submitted && Boolean(nameError)} aria-describedby={submitted && nameError ? "key-name-error" : undefined} onChange={(event) => setName(event.target.value)} />
        </Field>
        <Field id="key-days" label="Expires in days" error={submitted ? daysError : undefined}>
          <input id="key-days" className={inputClass} type="number" min={1} max={365} value={Number.isNaN(days) ? "" : days} aria-invalid={submitted && Boolean(daysError)} aria-describedby={submitted && daysError ? "key-days-error" : undefined} onChange={(event) => setDays(event.target.valueAsNumber)} />
        </Field>
      </div>
      {teamAdmin ? (
        <ChoiceGroup
          legend="Owner"
          name="key-scope"
          value={scope}
          onChange={(value) => {
            setScope(value);
            setSelected((current) => availableSelection(current, scopes.data, admin, value));
          }}
          choices={[
            { value: "user", label: "Personal", description: "Acts as you in any team you select with the request." },
            { value: "team", label: "Team", description: `Fixed to ${team?.name ?? "the current team"}. Team administrators can see and revoke it.` },
          ]}
        />
      ) : (
        <p className="text-xs text-fg-muted">This key is personal: it acts as you in any team you select with the request. Team administrators can also create team keys.</p>
      )}
      {scopes.isPending && <LoadingRows label="Loading permissions" rows={3} />}
      {scopes.isError && <QueryError what="permissions" error={scopes.error} onRetry={() => void scopes.refetch()} retrying={scopes.isFetching} />}
      {scopes.data && <ScopeChoices scopes={scopes.data} selected={selected} onToggle={toggle} unavailable={unavailable} />}
      {submitted && scopesError && (
        <p role="alert" className="text-xs text-bad">
          {scopesError}
        </p>
      )}
      {create.isError && (
        <Notice tone="error" title="Could not create API key">
          {errorMessage(create.error)}
        </Notice>
      )}
      <div className="flex justify-end">
        <Button type="submit" variant="primary" disabled={!online || bootstrap || !scopes.data} pending={create.isPending}>
          Create API key
        </Button>
      </div>
    </form>
  );
}

export function ApiKeyList() {
  const session = useSession();
  const online = useOnline();
  const now = useNow(60_000);
  const tokens = useQuery(accessTokensQuery);
  const revoke = useRevokeAccessToken();
  const [selected, setSelected] = useState<AccessToken | null>(null);
  const teamName = (id: string) => session?.teams.find((team) => team.id === id)?.name ?? id;

  return (
    <>
      {tokens.isPending && <LoadingRows label="Loading API keys" rows={2} />}
      {(tokens.isError || revoke.isError) && (
        <div className="space-y-2 p-3">
          {tokens.isError && <QueryError what="API keys" error={tokens.error} onRetry={() => void tokens.refetch()} retrying={tokens.isFetching} />}
          {revoke.isError && (
            <Notice tone="error" title="Could not revoke API key">
              {errorMessage(revoke.error)}
            </Notice>
          )}
        </div>
      )}
      {tokens.data?.length === 0 && <p className="px-4 py-3 text-[13px] text-fg-muted">No API keys created.</p>}
      {tokens.data && tokens.data.length > 0 && (
        <table className="w-full table-fixed text-left">
          <caption className="sr-only">API keys</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="pr-4 pl-4">Name</th>
              <th scope="col" className="hidden w-36 pr-4 text-right md:table-cell">Expires</th>
              <th scope="col" className="w-24 pr-4 text-right">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {tokens.data.map((token) => {
              const expired = Date.parse(token.expires_at) <= now;
              const owner = token.scope === "team" ? `Team · ${teamName(token.team_id)}` : "Personal";
              return (
                <tr key={token.id} className={tableRowClass}>
                  <td className="py-2 pr-4 pl-4 align-top">
                    <p className="text-[13px] font-medium break-words text-fg">{token.name}</p>
                    <p className="text-xs break-words text-fg-muted">
                      {owner}
                      {token.user_id !== session?.user_id && " · created by another member"}
                      <span className="md:hidden"> · {expired ? "Expired" : "Expires"} {formatAbsolute(token.expires_at)}</span>
                    </p>
                    <p className="mt-1 font-mono text-xs break-words text-fg-muted">{token.scopes.join(" ")}</p>
                    {expired && (
                      <p className="mt-1 text-xs">
                        <StatusLabel tone="warn" label="Expired" />
                      </p>
                    )}
                  </td>
                  <td className="hidden py-2 pr-4 text-right align-top text-[13px] whitespace-nowrap text-fg-muted tabular-nums md:table-cell">{formatAbsolute(token.expires_at)}</td>
                  <td className="py-2 pr-4 text-right align-top">
                    <Button size="sm" variant="danger" disabled={!online || revoke.isPending} onClick={() => setSelected(token)}>
                      Revoke<span className="sr-only"> {token.name}</span>
                    </Button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      <ConfirmDialog
        open={selected !== null}
        title={`Revoke ${selected?.name ?? "this API key"}?`}
        confirmLabel="Revoke key"
        danger
        onCancel={() => setSelected(null)}
        onConfirm={() => {
          if (selected) revoke.mutate(selected.id);
          setSelected(null);
        }}
      >
        Clients using this {selected ? scopeLabels[selected.scope].toLowerCase() : ""} key, including CLI contexts and MCP clients, lose access immediately. Operations already accepted continue.
      </ConfirmDialog>
    </>
  );
}
